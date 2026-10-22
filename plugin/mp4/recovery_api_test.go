package plugin_mp4

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	task "github.com/eanfs/gotask"
	m7s "m7s.live/v5"
	mp4pkg "m7s.live/v5/plugin/mp4/pkg"
)

// newRecoveryAPITestPlugin 组装一个未启动的插件 + 恢复任务，用于 handler 单元测试。
// 未启动的任务仍可用于 list/目录白名单；run 需要已启动的任务（见 startRecoveryAPITestTask）。
func newRecoveryAPITestPlugin(t *testing.T, dirs ...string) (*MP4Plugin, *IndexRecoveryTask) {
	t.Helper()
	p := &MP4Plugin{}
	recovery := &IndexRecoveryTask{Dirs: dirs, plugin: p}
	recovery.observer = p.observeRecovery
	p.recoveryTask.Store(recovery)
	return p, recovery
}

// startRecoveryAPITestTask 把恢复任务挂到根任务管理器上，使 AddTask 路径可用。
func startRecoveryAPITestTask(t *testing.T, recovery *IndexRecoveryTask) {
	t.Helper()
	var root task.RootManager[uint32, *task.Work]
	root.Init()
	// 与 TestIndexRecoveryTaskConcurrentRunOnce 一致：不 root.Stop，规避 gotask
	// v1.0.5 已知的销毁竞态（CLAUDE.md 已记录）。
	if err := root.AddTask(recovery).WaitStarted(); err != nil {
		t.Fatalf("start recovery task: %v", err)
	}
}

func doRecoveryRequest(t *testing.T, handler http.HandlerFunc, method, target string,
	body []byte, pathValues map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	for key, value := range pathValues {
		req.SetPathValue(key, value)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

type recoveryListResponse struct {
	Code      int  `json:"code"`
	Page      int  `json:"page"`
	Size      int  `json:"size"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	Counts    struct {
		Pending    int `json:"pending"`
		Recovering int `json:"recovering"`
		Failed     int `json:"failed"`
	} `json:"counts"`
	Data struct {
		Pending    []recoveryListEntry   `json:"pending"`
		Recovering []recoveryListEntry   `json:"recovering"`
		Failed     []recoveryListEntry   `json:"failed"`
		Recent     []recoveryRecentEntry `json:"recent"`
	} `json:"data"`
}

// Step 1: list 返回可解析的 JSON，并按 pending/recovering/failed 分类。
func TestRecoveryListJSONShape(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.mp4.m7sidx", "b.mp4.m7sidx.recovering", "c.mp4.m7sidx.failed"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	p, _ := newRecoveryAPITestPlugin(t, dir)

	rec := doRecoveryRequest(t, p.handleRecoveryList, http.MethodGet, "/api/recovery/list", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got recoveryListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode list: %v (body=%s)", err, rec.Body.String())
	}
	if got.Code != 0 || got.Page != 1 || got.Size != recoveryListDefaultPageSize {
		t.Fatalf("unexpected envelope: %+v", got)
	}
	if got.Total != 3 {
		t.Fatalf("total = %d, want 3", got.Total)
	}
	if got.Counts.Pending != 1 || got.Counts.Recovering != 1 || got.Counts.Failed != 1 {
		t.Fatalf("counts = %+v", got.Counts)
	}
	if len(got.Data.Pending) != 1 || len(got.Data.Recovering) != 1 || len(got.Data.Failed) != 1 {
		t.Fatalf("data lengths = %d/%d/%d", len(got.Data.Pending), len(got.Data.Recovering), len(got.Data.Failed))
	}
	if got.Data.Pending[0].MediaPath != filepath.Join(dir, "a.mp4") {
		t.Fatalf("mediaPath = %q", got.Data.Pending[0].MediaPath)
	}
	if got.Data.Recent == nil {
		t.Fatal("recent must be an empty array, not null")
	}
}

// 分页：size 被夹紧到上限，page 超出范围返回空数组而不是全量。
func TestRecoveryListPaginationClamped(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		name := filepath.Join(dir, "s"+string(rune('a'+i))+".mp4.m7sidx")
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	p, _ := newRecoveryAPITestPlugin(t, dir)

	rec := doRecoveryRequest(t, p.handleRecoveryList, http.MethodGet,
		"/api/recovery/list?page=2&size=9999", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got recoveryListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Size != recoveryListMaxPageSize {
		t.Fatalf("size = %d, want clamp %d", got.Size, recoveryListMaxPageSize)
	}
	if got.Total != 5 || len(got.Data.Pending) != 0 {
		t.Fatalf("total=%d pending=%d, want total 5, empty page 2", got.Total, len(got.Data.Pending))
	}
}

// Step 4: 白名单外目录返回 403，且必须是 403 而不是 400。
func TestRecoveryRunRejectsDirOutsideAllowList(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()
	p, _ := newRecoveryAPITestPlugin(t, allowed)

	body, _ := json.Marshal(map[string]string{"dir": outside})
	rec := doRecoveryRequest(t, p.handleRecoveryRun, http.MethodPost, "/api/recovery/run", body, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
}

// 路径穿越/符号链接逃逸必须同样被拒绝。
func TestRecoveryRunRejectsSymlinkEscape(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(allowed, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	p, _ := newRecoveryAPITestPlugin(t, allowed)

	body, _ := json.Marshal(map[string]string{"dir": link})
	rec := doRecoveryRequest(t, p.handleRecoveryRun, http.MethodPost, "/api/recovery/run", body, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}

	traversal := filepath.Join(allowed, "..", filepath.Base(outside))
	body, _ = json.Marshal(map[string]string{"dir": traversal})
	rec = doRecoveryRequest(t, p.handleRecoveryRun, http.MethodPost, "/api/recovery/run", body, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("traversal status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
}

// status 未知 id 返回 404。
func TestRecoveryStatusUnknownID(t *testing.T) {
	p, _ := newRecoveryAPITestPlugin(t, t.TempDir())
	rec := doRecoveryRequest(t, p.handleRecoveryStatus, http.MethodGet,
		"/api/recovery/status/nope", nil, map[string]string{"id": "nope"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
}

// 鉴权开启且缺少 Authorization 时返回 401。
func TestRecoveryListRequiresAdminWhenLoginEnabled(t *testing.T) {
	p, _ := newRecoveryAPITestPlugin(t, t.TempDir())
	p.Server = &m7s.Server{}
	p.Server.ServerConfig.Admin.EnableLogin = true

	rec := doRecoveryRequest(t, p.handleRecoveryList, http.MethodGet, "/api/recovery/list", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body = %s", rec.Code, rec.Body.String())
	}
}

type recoveryRunResponse struct {
	Code         int    `json:"code"`
	ID           string `json:"id"`
	Status       string `json:"status"`
	Deduplicated bool   `json:"deduplicated"`
}

type recoveryStatusResponse struct {
	Code     int               `json:"code"`
	ID       string            `json:"id"`
	Status   string            `json:"status"`
	Progress recoveryStatsView `json:"progress"`
	Result   *struct {
		Scanned   int      `json:"scanned"`
		Recovered int      `json:"recovered"`
		Failed    int      `json:"failed"`
		Skipped   int      `json:"skipped"`
		Processed int      `json:"processed"`
		Errors    []string `json:"errors"`
	} `json:"result"`
	Error string `json:"error"`
}

// 成功路径：run 返回 task id，status 轮询到 completed，结果计数正确。
func TestRecoveryRunSuccessAndStatus(t *testing.T) {
	dir := t.TempDir()
	buildRecoveryFixture(t, dir, "a.tmp", []byte{0x00, 0x01, 0x02, 0x03})

	p, recovery := newRecoveryAPITestPlugin(t, dir)
	recovery.EnqueueUpload = func(RecoveryItem) error { return nil }
	startRecoveryAPITestTask(t, recovery)

	body, _ := json.Marshal(map[string]string{"dir": dir})
	rec := doRecoveryRequest(t, p.handleRecoveryRun, http.MethodPost, "/api/recovery/run", body, nil)
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("run status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var run recoveryRunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.ID == "" {
		t.Fatalf("empty run id: %s", rec.Body.String())
	}

	deadline := time.Now().Add(10 * time.Second)
	var status recoveryStatusResponse
	for {
		srec := doRecoveryRequest(t, p.handleRecoveryStatus, http.MethodGet,
			"/api/recovery/status/"+run.ID, nil, map[string]string{"id": run.ID})
		if srec.Code != http.StatusOK {
			t.Fatalf("status code = %d, body = %s", srec.Code, srec.Body.String())
		}
		if err := json.Unmarshal(srec.Body.Bytes(), &status); err != nil {
			t.Fatalf("decode status: %v", err)
		}
		if status.Status != recoveryRunRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovery run did not finish in time")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if status.Status != recoveryRunCompleted {
		t.Fatalf("status = %q, error = %q", status.Status, status.Error)
	}
	if status.Result == nil || status.Result.Scanned != 1 || status.Result.Recovered != 1 {
		t.Fatalf("result = %+v", status.Result)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.mp4")); err != nil {
		t.Fatalf("recovered output missing: %v", err)
	}

	// 最近恢复结果应可通过 list 查看。
	lrec := doRecoveryRequest(t, p.handleRecoveryList, http.MethodGet, "/api/recovery/list", nil, nil)
	var list recoveryListResponse
	if err := json.Unmarshal(lrec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Data.Recent) == 0 {
		t.Fatal("expected recent recovery entry")
	}
	found := false
	for _, entry := range list.Data.Recent {
		if entry.File == filepath.Join(dir, "a.mp4") {
			found = true
		}
	}
	if !found {
		t.Fatalf("recent entries = %+v", list.Data.Recent)
	}
}

// 手动 run 不得绕过 record.recovery.minage：mtime 新鲜（疑似活跃录制）的文件跳过。
func TestRecoveryRunSkipsFreshlyWrittenFile(t *testing.T) {
	dir := t.TempDir()
	media, sidecar, _ := buildRecoveryFixture(t, dir, "a.tmp", []byte{0x00, 0x01, 0x02, 0x03})
	now := time.Now()
	if err := os.Chtimes(media, now, now); err != nil {
		t.Fatalf("touch media: %v", err)
	}
	if err := os.Chtimes(sidecar, now, now); err != nil {
		t.Fatalf("touch sidecar: %v", err)
	}

	p, recovery := newRecoveryAPITestPlugin(t, dir)
	recovery.EnqueueUpload = func(RecoveryItem) error { return nil }
	startRecoveryAPITestTask(t, recovery)

	body, _ := json.Marshal(map[string]string{"file": sidecar})
	rec := doRecoveryRequest(t, p.handleRecoveryRun, http.MethodPost, "/api/recovery/run", body, nil)
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("run status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var run recoveryRunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	var status recoveryStatusResponse
	for {
		srec := doRecoveryRequest(t, p.handleRecoveryStatus, http.MethodGet,
			"/api/recovery/status/"+run.ID, nil, map[string]string{"id": run.ID})
		if err := json.Unmarshal(srec.Body.Bytes(), &status); err != nil {
			t.Fatalf("decode status: %v", err)
		}
		if status.Status != recoveryRunRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovery run did not finish in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status.Result == nil || status.Result.Skipped != 1 || status.Result.Recovered != 0 {
		t.Fatalf("result = %+v, want skipped=1 recovered=0", status.Result)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.mp4")); err == nil {
		t.Fatal("fresh file must not be recovered")
	}
}

// 运行中重复 POST 相同目标应返回同一 id（幂等），不同目标返回 409。
func TestRecoveryRunDeduplicatesInFlight(t *testing.T) {
	dir := t.TempDir()
	p, recovery := newRecoveryAPITestPlugin(t, dir)
	startRecoveryAPITestTask(t, recovery)

	canonicalDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	run := &recoveryRunState{id: "rec-test", key: "dir:" + canonicalDir, status: recoveryRunRunning, startedAt: time.Now()}
	p.recoveryState.mu.Lock()
	p.recoveryState.runs = map[string]*recoveryRunState{run.id: run}
	p.recoveryState.current = run
	p.recoveryState.mu.Unlock()

	body, _ := json.Marshal(map[string]string{"dir": dir})
	rec := doRecoveryRequest(t, p.handleRecoveryRun, http.MethodPost, "/api/recovery/run", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dedup status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got recoveryRunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != "rec-test" || !got.Deduplicated {
		t.Fatalf("dedup response = %+v", got)
	}

	other := t.TempDir()
	p2, p2Recovery := newRecoveryAPITestPlugin(t, dir, other)
	startRecoveryAPITestTask(t, p2Recovery)
	p2.recoveryState.mu.Lock()
	p2.recoveryState.runs = p.recoveryState.runs
	p2.recoveryState.current = run
	p2.recoveryState.mu.Unlock()
	body, _ = json.Marshal(map[string]string{"dir": other})
	rec = doRecoveryRequest(t, p2.handleRecoveryRun, http.MethodPost, "/api/recovery/run", body, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// waitRecoveryRun 轮询 status 直到运行结束。
func waitRecoveryRun(t *testing.T, p *MP4Plugin, id string) recoveryStatusResponse {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		srec := doRecoveryRequest(t, p.handleRecoveryStatus, http.MethodGet,
			"/api/recovery/status/"+id, nil, map[string]string{"id": id})
		if srec.Code != http.StatusOK {
			t.Fatalf("status code = %d, body = %s", srec.Code, srec.Body.String())
		}
		var status recoveryStatusResponse
		if err := json.Unmarshal(srec.Body.Bytes(), &status); err != nil {
			t.Fatalf("decode status: %v", err)
		}
		if status.Status != recoveryRunRunning {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatal("recovery run did not finish in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// I1：sidecar 合法但索引 header.mediaPath 指向白名单外时，必须在触发前
// 拒绝（403），且白名单外的目标文件不得被覆盖/删除。
func TestRecoveryRunRejectsIndexMediaPathOutsideAllowList(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()

	media, payloadStart := writeHalfWrittenRecording(t, outside, "valuable.tmp", []byte{0x11, 0x22, 0x33, 0x44})
	ageRecoveryFiles(t, media)
	original, err := os.ReadFile(media)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}

	sidecar := filepath.Join(allowed, "evil.tmp"+mp4pkg.IndexExtension)
	writeRecoverySidecar(t, sidecar, mp4pkg.IndexHeader{
		Version:     1,
		MediaPath:   media,
		StorageType: "local",
		Tracks: []mp4pkg.IndexTrack{{
			ID: 1, FourCC: "avc1", Width: 320, Height: 240, STSD: recoveryTestSTSD(t),
		}},
	}, []mp4pkg.IndexRecord{
		{Flags: 0x01, TrackID: 1, Size: 4, Offset: uint64(payloadStart), DTS: 0, Duration: 40},
	})
	ageRecoveryFiles(t, sidecar)

	p, recovery := newRecoveryAPITestPlugin(t, allowed)
	recovery.EnqueueUpload = func(RecoveryItem) error { return nil }
	startRecoveryAPITestTask(t, recovery)

	body, _ := json.Marshal(map[string]string{"file": sidecar})
	rec := doRecoveryRequest(t, p.handleRecoveryRun, http.MethodPost, "/api/recovery/run", body, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
	if got, err := os.ReadFile(media); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("target outside allow-list was modified: err=%v", err)
	}
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("sidecar must be left untouched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "valuable.mp4")); !os.IsNotExist(err) {
		t.Fatal("no output may be written outside the allow-list")
	}
}

// I1：白名单内的 sidecar 若是符号链接（指向白名单外），一律拒绝。
func TestRecoveryRunRejectsSymlinkedSidecar(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()
	media, sidecar, _ := buildRecoveryFixture(t, outside, "a.tmp", []byte{0x00, 0x01, 0x02, 0x03})

	link := filepath.Join(allowed, "link.mp4"+mp4pkg.IndexExtension)
	if err := os.Symlink(sidecar, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	original, err := os.ReadFile(media)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}

	p, recovery := newRecoveryAPITestPlugin(t, allowed)
	recovery.EnqueueUpload = func(RecoveryItem) error { return nil }
	startRecoveryAPITestTask(t, recovery)

	body, _ := json.Marshal(map[string]string{"file": link})
	rec := doRecoveryRequest(t, p.handleRecoveryRun, http.MethodPost, "/api/recovery/run", body, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
	if got, err := os.ReadFile(media); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("media outside allow-list was modified: err=%v", err)
	}
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("sidecar outside allow-list must be untouched: %v", err)
	}
}

// I1：索引记录的媒体路径本身是白名单内指向外部的符号链接时同样拒绝。
func TestRecoveryRunRejectsMediaSymlinkEscape(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()

	target, _ := writeHalfWrittenRecording(t, outside, "valuable.tmp", []byte{0x01, 0x02, 0x03, 0x04})
	ageRecoveryFiles(t, target)
	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}

	link := filepath.Join(allowed, "media.mp4")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	sidecar := filepath.Join(allowed, "media.mp4"+mp4pkg.IndexExtension)
	writeRecoverySidecar(t, sidecar, mp4pkg.IndexHeader{
		Version: 1, MediaPath: link, StorageType: "local",
		Tracks: []mp4pkg.IndexTrack{{
			ID: 1, FourCC: "avc1", Width: 320, Height: 240, STSD: recoveryTestSTSD(t),
		}},
	}, []mp4pkg.IndexRecord{
		{Flags: 0x01, TrackID: 1, Size: 4, Offset: 0, DTS: 0, Duration: 40},
	})
	ageRecoveryFiles(t, sidecar)

	p, recovery := newRecoveryAPITestPlugin(t, allowed)
	recovery.EnqueueUpload = func(RecoveryItem) error { return nil }
	startRecoveryAPITestTask(t, recovery)

	body, _ := json.Marshal(map[string]string{"file": sidecar})
	rec := doRecoveryRequest(t, p.handleRecoveryRun, http.MethodPost, "/api/recovery/run", body, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("symlink target was modified: err=%v", err)
	}
}

// 合法路径不受影响：白名单内的 sidecar 且 mediaPath 在同一目录时必须能恢复。
func TestRecoveryRunFileWithinAllowListRecovers(t *testing.T) {
	dir := t.TempDir()
	_, sidecar, _ := buildRecoveryFixture(t, dir, "a.tmp", []byte{0x00, 0x01, 0x02, 0x03})

	p, recovery := newRecoveryAPITestPlugin(t, dir)
	recovery.EnqueueUpload = func(RecoveryItem) error { return nil }
	startRecoveryAPITestTask(t, recovery)

	body, _ := json.Marshal(map[string]string{"file": sidecar})
	rec := doRecoveryRequest(t, p.handleRecoveryRun, http.MethodPost, "/api/recovery/run", body, nil)
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("run status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var run recoveryRunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}

	status := waitRecoveryRun(t, p, run.ID)
	if status.Status != recoveryRunCompleted {
		t.Fatalf("status = %q, error = %q", status.Status, status.Error)
	}
	if status.Result == nil || status.Result.Recovered != 1 {
		t.Fatalf("result = %+v", status.Result)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.mp4")); err != nil {
		t.Fatalf("recovered output missing: %v", err)
	}
}

// I2：启动扫描在途（持有 runMu）时触发的手动 run，不得把扫描的逐文件
// 结果累计进自己的 progress；结束后以真实统计整体覆盖。
func TestRecoveryRunProgressIgnoresInFlightStartupScan(t *testing.T) {
	dir := t.TempDir()
	p, recovery := newRecoveryAPITestPlugin(t, dir)
	startRecoveryAPITestTask(t, recovery)

	// 模拟启动扫描：持有 runMu 直到测试主动释放。
	recovery.runMu.Lock()
	locked := true
	defer func() {
		if locked {
			recovery.runMu.Unlock()
		}
	}()

	body, _ := json.Marshal(map[string]string{"dir": dir})
	rec := doRecoveryRequest(t, p.handleRecoveryRun, http.MethodPost, "/api/recovery/run", body, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var run recoveryRunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}

	// 启动扫描的逐文件结果（runID 为空）。
	for i := 0; i < 3; i++ {
		p.observeRecovery("", filepath.Join(dir, fmt.Sprintf("scan-%d.mp4", i)), "recovered", nil, 10, false)
	}

	srec := doRecoveryRequest(t, p.handleRecoveryStatus, http.MethodGet,
		"/api/recovery/status/"+run.ID, nil, map[string]string{"id": run.ID})
	var status recoveryStatusResponse
	if err := json.Unmarshal(srec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.Status != recoveryRunRunning {
		t.Fatalf("status = %q, want running", status.Status)
	}
	if status.Progress.Recovered != 0 || status.Progress.Processed != 0 {
		t.Fatalf("progress polluted by in-flight startup scan: %+v", status.Progress)
	}

	recovery.runMu.Unlock()
	locked = false

	status = waitRecoveryRun(t, p, run.ID)
	if status.Status != recoveryRunCompleted {
		t.Fatalf("status = %q, error = %q", status.Status, status.Error)
	}
	if status.Result == nil || status.Result.Scanned != 0 || status.Result.Recovered != 0 {
		t.Fatalf("result must reflect the manual run only: %+v", status.Result)
	}
}
