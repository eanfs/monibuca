package plugin_mp4

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	task "github.com/eanfs/gotask"
	mp4pkg "m7s.live/v5/plugin/mp4/pkg"
)

// canonPath 返回路径解析符号链接后的真实路径。恢复管线对外报告/下发的路径
// 来自 guard，均已 canon 化；macOS 上 t.TempDir() 的 /var 会解析成 /private/var，
// 断言必须用同一坐标系。
func canonPath(t *testing.T, path string) string {
	t.Helper()
	canon, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("eval symlinks %s: %v", path, err)
	}
	return canon
}

// ---------------------------------------------------------------- 路径 guard：返回 canon

// 白名单校验通过后必须返回**解析符号链接后的真实路径**，而不是原路径：
// 调用方拿返回值去 rename/覆盖/删除，原路径在检查与使用之间可被替换成指向
// 别处的链接（TOCTOU）。
func TestRecoveryGuardReturnsCanonicalPaths(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	media := filepath.Join(realDir, "media.tmp")
	if err := os.WriteFile(media, []byte("media"), 0o644); err != nil {
		t.Fatalf("write media: %v", err)
	}
	sidecar := media + mp4pkg.IndexExtension
	if err := os.WriteFile(sidecar, []byte("sidecar"), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	// 白名单内的一条链接：<base>/link -> <base>/real。
	// 通过它给出路径时，check 通过（目标在白名单内）但返回值必须落到 real。
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	guard, err := newRecoveryPathGuard([]string{realDir}, sidecar)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}

	wantMedia, err := filepath.EvalSymlinks(media)
	if err != nil {
		t.Fatalf("eval media: %v", err)
	}
	got, err := guard.allowExisting(filepath.Join(link, "media.tmp"), "recovery media")
	if err != nil {
		t.Fatalf("allowExisting: %v", err)
	}
	if got != wantMedia {
		t.Fatalf("allowExisting = %q, want canonical %q", got, wantMedia)
	}

	wantOutput := filepath.Join(filepath.Dir(wantMedia), "media.mp4")
	output, err := guard.allowOutput(filepath.Join(link, "media.mp4"), "recovery output")
	if err != nil {
		t.Fatalf("allowOutput: %v", err)
	}
	if output != wantOutput {
		t.Fatalf("allowOutput = %q, want canonical %q", output, wantOutput)
	}
}

// ---------------------------------------------------------------- run handler 不阻塞

// recoveryLoopStallTask 让事件循环阻塞在 Run 里：Run 内联跑在父 Job 的事件
// 循环上，用它模拟「子任务已入队但事件循环无法继续启动下一个任务」。
type recoveryLoopStallTask struct {
	task.Task
	release <-chan struct{}
}

func (s *recoveryLoopStallTask) Run() error {
	<-s.release
	return task.ErrTaskComplete
}

// run handler 只登记任务并立即返回，绝不在请求内等待任务启动：事件循环被
// 阻塞时 WaitStarted 可能永不返回，同步等待会让 HTTP 连接永久挂住。
func TestRecoveryRunHandlerDoesNotWaitForTaskStart(t *testing.T) {
	dir := t.TempDir()
	p, recovery := newRecoveryAPITestPlugin(t, dir)
	startRecoveryAPITestTask(t, recovery)

	release := make(chan struct{})
	defer close(release)
	if err := recovery.AddTask(&recoveryLoopStallTask{release: release}).WaitStarted(); err != nil {
		t.Fatalf("start stall task: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"dir": dir})
	type handlerResult struct {
		code int
		body string
	}
	done := make(chan handlerResult, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/recovery/run", bytes.NewReader(body))
		req.RemoteAddr = "127.0.0.1:54321"
		rec := httptest.NewRecorder()
		p.handleRecoveryRun(rec, req)
		done <- handlerResult{code: rec.Code, body: rec.Body.String()}
	}()

	select {
	case got := <-done:
		if got.code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202, body = %s", got.code, got.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("recovery run handler blocked waiting for task start; it must answer 202 immediately")
	}
}

// ---------------------------------------------------------------- 启动观测 watcher

// 子任务启动失败时必须把 run 标记为 failed 并记录原因，否则 /status 永远
// 停在 running（handler 已经返回 202，没有别的地方会收尾）。
func TestRecoveryStartWatcherMarksRunFailed(t *testing.T) {
	state := &recoveryAPIState{}
	run := &recoveryRunState{id: "rec-start-failure", key: "dir:<default>", status: recoveryRunRunning, startedAt: time.Now()}
	state.mu.Lock()
	state.ensureRunsLocked()
	state.runs[run.id] = run
	state.order = append(state.order, run.id)
	state.current = run
	state.mu.Unlock()

	watcher := &recoveryStartWatcher{
		waitStarted: func() error { return errors.New("event loop stopped") },
		state:       state,
		run:         run,
	}
	if err := watcher.Go(); !errors.Is(err, task.ErrTaskComplete) {
		t.Fatalf("watcher Go = %v, want ErrTaskComplete", err)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if run.status != recoveryRunFailed {
		t.Fatalf("run status = %q, want %q", run.status, recoveryRunFailed)
	}
	if run.finishedAt.IsZero() {
		t.Fatal("failed run must carry finishedAt")
	}
	if state.current != nil {
		t.Fatalf("in-flight run must be cleared, got %+v", state.current)
	}
	if !strings.Contains(run.err, "event loop stopped") {
		t.Fatalf("run error = %q, want it to record the start failure", run.err)
	}
}

// 启动成功时 watcher 不做任何收尾：run 继续由 recoveryAPIJob 执行并 finishRun。
func TestRecoveryStartWatcherLeavesSuccessfulRunRunning(t *testing.T) {
	state := &recoveryAPIState{}
	run := &recoveryRunState{id: "rec-start-ok", key: "dir:<default>", status: recoveryRunRunning, startedAt: time.Now()}
	state.mu.Lock()
	state.ensureRunsLocked()
	state.runs[run.id] = run
	state.order = append(state.order, run.id)
	state.current = run
	state.mu.Unlock()

	watcher := &recoveryStartWatcher{
		waitStarted: func() error { return nil },
		state:       state,
		run:         run,
	}
	if err := watcher.Go(); !errors.Is(err, task.ErrTaskComplete) {
		t.Fatalf("watcher Go = %v, want ErrTaskComplete", err)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if run.status != recoveryRunRunning || !run.finishedAt.IsZero() {
		t.Fatalf("run = %+v, want still running", run)
	}
	if state.current != run {
		t.Fatal("successful start must keep the run in flight")
	}
}

// ---------------------------------------------------------------- 扫描条目上限

// 上限必须按「遍历到的条目总数」计（含与恢复无关的普通文件），否则一个
// 塞满普通文件的目录每次 list 都会全树递归，上限形同虚设。
func TestScanRecoveryEntriesCountsAllVisitedEntries(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	// 字典序最后，只有走到它才会被收集。
	if err := os.WriteFile(filepath.Join(dir, "d.mp4.m7sidx"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	entries, truncated, errs := scanRecoveryEntries([]string{dir}, 3)
	if len(errs) != 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if !truncated {
		t.Fatal("scan must report truncated when the entry budget is exhausted")
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %+v, want none (budget exhausted before the sidecar)", entries)
	}
}

// 恢复扫描（discoverSidecars）与 list 共用「普通文件同样消耗遍历预算」的语义：
// 超限提前返回并报告截断。收集上限与遍历预算分开，这里用小预算直接钉住遍历口径。
func TestDiscoverSidecarsCountsAllVisitedEntries(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "d.mp4.m7sidx"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	files, bareTmp, truncated, errs := discoverSidecars([]string{dir}, 3, recoveryScanEntryLimit)
	if len(errs) != 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if truncated != sidecarScanTruncatedVisited {
		t.Fatalf("truncated = %q, want %q (walk budget exhausted)", truncated, sidecarScanTruncatedVisited)
	}
	if len(files) != 0 || bareTmp != 0 {
		t.Fatalf("files = %v, bareTmp = %d, want none", files, bareTmp)
	}
}

// Important 1（回归）：遍历预算不得按「与 sidecar 无关的条目」先耗尽。
//
// 默认扫描目录包含整个录像根目录，文件名带时间戳前缀、WalkDir 按字典序遍历。
// 若遍历预算仍是 1 万条，累计超过 1 万条目后扫描会稳定停在字典序靠前的旧文件，
// 最新录像（字典序最后）每次启动/周期重扫都发现不了。遍历预算必须远大于
// 收集上限，让「条目多」不再等于「新录像被饿死」。
func TestDiscoverSidecarsDoesNotStarveNewestRecording(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < recoveryScanEntryLimit+1; i++ {
		name := filepath.Join(dir, fmt.Sprintf("f%05d.bin", i))
		if err := os.WriteFile(name, nil, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	// 字典序排在所有普通文件之后 = 最新录像：半成品 + sidecar，应被真正恢复。
	buildRecoveryFixture(t, dir, "zz_newest.tmp", []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04})

	var items []RecoveryItem
	recovery := &IndexRecoveryTask{EnqueueUpload: func(item RecoveryItem) error {
		items = append(items, item)
		return nil
	}}
	stats, err := recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Recovered != 1 || len(items) != 1 {
		t.Fatalf("newest sidecar was starved by the walk budget: stats = %+v, enqueued = %d",
			stats, len(items))
	}
}

// 收集上限单独可观测：sidecar 数量达到 collectLimit 时报告 collected 截断，
// 与遍历预算耗尽区分开（日志据此指明是哪个上限）。
func TestDiscoverSidecarsReportsCollectedLimit(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.mp4.m7sidx", "b.mp4.m7sidx", "c.mp4.m7sidx"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	files, _, truncated, errs := discoverSidecars([]string{dir}, recoveryVisitedEntryLimit, 2)
	if len(errs) != 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if truncated != sidecarScanTruncatedCollected {
		t.Fatalf("truncated = %q, want %q (collected budget exhausted)", truncated, sidecarScanTruncatedCollected)
	}
	if len(files) != 2 {
		t.Fatalf("files = %v, want exactly the collect cap (2)", files)
	}
}

// list 端点必须按默认上限（recoveryScanEntryLimit）截断：目录里塞满普通文件时
// 也只遍历到上限为止，并如实返回 truncated=true。
func TestRecoveryListTruncatesAtDefaultScanLimit(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < recoveryScanEntryLimit+1; i++ {
		name := filepath.Join(dir, fmt.Sprintf("f%05d.bin", i))
		if err := os.WriteFile(name, nil, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	// 字典序排在普通文件之后：预算耗尽时它不会被扫到。
	if err := os.WriteFile(filepath.Join(dir, "zz.mp4.m7sidx"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	p, _ := newRecoveryAPITestPlugin(t, dir)
	rec := doRecoveryRequest(t, p.handleRecoveryList, http.MethodGet, "/api/recovery/list", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got recoveryListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if !got.Truncated {
		t.Fatal("list must report truncated=true once the entry budget is exhausted")
	}
	if got.Total != 0 {
		t.Fatalf("total = %d, want 0 (nothing may be scanned past the budget)", got.Total)
	}
}

// ---------------------------------------------------------------- 鉴权 fail-closed

// 插件未绑定 Server 时无法判定是否开启登录、也无法查角色，必须 fail-closed：
// 三个端点一律 403，而不是按“未开启登录”放行。
func TestRecoveryHandlersRejectUnboundServer(t *testing.T) {
	p := &MP4Plugin{} // Server == nil
	p.recoveryTask.Store(&IndexRecoveryTask{})

	cases := []struct {
		name    string
		handler http.HandlerFunc
		method  string
		target  string
		paths   map[string]string
	}{
		{"list", p.handleRecoveryList, http.MethodGet, "/api/recovery/list", nil},
		{"run", p.handleRecoveryRun, http.MethodPost, "/api/recovery/run", nil},
		{"status", p.handleRecoveryStatus, http.MethodGet, "/api/recovery/status/x", map[string]string{"id": "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRecoveryRequest(t, tc.handler, tc.method, tc.target, nil, tc.paths)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// owner 已停止时两个 AddTask 都会被立即拒绝、watcher 永远不会启动：startRun
// 必须同步收尾（run 标 failed、释放 in-flight）并返回错误。否则该 run 会永远
// 停在 running，且后续所有请求都被判 conflict。
func TestRecoveryStartRunFailsWhenOwnerStopped(t *testing.T) {
	var root task.RootManager[uint32, *task.Work]
	root.Init()
	owner := &IndexRecoveryTask{}
	if err := root.AddTask(owner).WaitStarted(); err != nil {
		t.Fatalf("start recovery task: %v", err)
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		owner.Stop(errors.New("shutdown"))
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stopping the recovery task blocked")
	}

	state := &recoveryAPIState{}
	id, _, _, err := state.startRun(owner, nil, "")
	if err == nil || !strings.Contains(err.Error(), "shutdown") {
		t.Fatalf("startRun error = %v, want it to carry the stop reason", err)
	}
	run, ok := state.getRun(id)
	if !ok {
		t.Fatalf("run %s must be registered before the failure", id)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if run.status != recoveryRunFailed || run.finishedAt.IsZero() {
		t.Fatalf("run = %+v, want failed with finishedAt", run)
	}
	if state.current != nil {
		t.Fatalf("in-flight run must be cleared, got %+v", state.current)
	}
}

// canon 化不得放宽白名单：经由白名单内的符号链接指向外部时，读/写两侧
// 都必须继续拒绝（errRecoveryPathDenied）。
func TestRecoveryGuardRejectsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	inside := filepath.Join(base, "inside")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{inside, outside} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	sidecar := filepath.Join(inside, "media.tmp"+mp4pkg.IndexExtension)
	if err := os.WriteFile(sidecar, []byte("x"), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	link := filepath.Join(inside, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	guard, err := newRecoveryPathGuard([]string{inside}, sidecar)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if _, err := guard.allowExisting(filepath.Join(link, "secret"), "recovery media"); !errors.Is(err, errRecoveryPathDenied) {
		t.Fatalf("allowExisting err = %v, want errRecoveryPathDenied", err)
	}
	if _, err := guard.allowOutput(filepath.Join(link, "out.mp4"), "recovery output"); !errors.Is(err, errRecoveryPathDenied) {
		t.Fatalf("allowOutput err = %v, want errRecoveryPathDenied", err)
	}
}
