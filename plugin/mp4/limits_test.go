package plugin_mp4

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	task "github.com/eanfs/gotask"
	m7s "m7s.live/v5"
	"m7s.live/v5/pkg/config"
	mp4pb "m7s.live/v5/plugin/mp4/pb"
)

// waitFor 在 deadline 前轮询 ready，用于替代固定 sleep（确定性测试）。
func waitFor(t *testing.T, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func acquireAsync(l *RecordLimiter, ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() { done <- l.Acquire(ctx) }()
	return done
}

// 上限 1、并发 3：第 2 个排队，Release 后放行；第 3 个排队超时返回错误。
func TestRecordLimiterQueuesThenTimesOut(t *testing.T) {
	l := NewRecordLimiter(1)
	l.timeout = 30 * time.Millisecond

	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	second := acquireAsync(l, context.Background())
	waitFor(t, "second waiter to queue", func() bool { return l.Waiting() == 1 })

	third := acquireAsync(l, context.Background())
	waitFor(t, "third waiter to queue", func() bool { return l.Waiting() == 2 })

	l.Release()
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("second acquire should be admitted after release, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second acquire did not return after release")
	}
	if got := l.Active(); got != 1 {
		t.Fatalf("active = %d after handover, want 1", got)
	}

	select {
	case err := <-third:
		if !errors.Is(err, ErrRecordLimitTimeout) {
			t.Fatalf("third acquire error = %v, want ErrRecordLimitTimeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("third acquire did not time out")
	}

	// 超时的等待者不得占用名额。
	if got := l.Waiting(); got != 0 {
		t.Fatalf("waiting = %d after timeout, want 0", got)
	}
	l.Release()
	if got := l.Active(); got != 0 {
		t.Fatalf("active = %d after final release, want 0", got)
	}
}

// Release 必须唤醒等待者；槽位在释放者与等待者之间转移而不会凭空减少。
func TestRecordLimiterReleaseLetsWaiterProceed(t *testing.T) {
	l := NewRecordLimiter(1)
	l.timeout = time.Second
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	waiter := acquireAsync(l, context.Background())
	waitFor(t, "waiter to queue", func() bool { return l.Waiting() == 1 })

	l.Release()
	select {
	case err := <-waiter:
		if err != nil {
			t.Fatalf("waiter acquire: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter was not woken by release")
	}
	if got, want := l.Active(), 1; got != want {
		t.Fatalf("active = %d, want %d", got, want)
	}
	l.Release()
	if got := l.Active(); got != 0 {
		t.Fatalf("active = %d after release, want 0", got)
	}
}

// 有界等待队列：容量满后立即拒绝，不无限排队。
func TestRecordLimiterQueueIsBounded(t *testing.T) {
	l := NewRecordLimiter(1)
	l.timeout = time.Second
	if want := 1 * recordLimitQueueFactor; l.queueCapacity != want {
		t.Fatalf("queueCapacity = %d, want %d", l.queueCapacity, want)
	}
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	results := make([]<-chan error, 0, l.queueCapacity)
	for i := 0; i < l.queueCapacity; i++ {
		results = append(results, acquireAsync(l, ctx))
	}
	waitFor(t, "queue to fill", func() bool { return l.Waiting() == l.queueCapacity })

	if err := l.Acquire(context.Background()); !errors.Is(err, ErrRecordLimitQueueFull) {
		t.Fatalf("acquire with full queue = %v, want ErrRecordLimitQueueFull", err)
	}

	cancel() // 释放所有排队者（返回 ctx.Canceled）
	for _, result := range results {
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("canceled waiter should return an error")
			}
		case <-time.After(time.Second):
			t.Fatal("canceled waiter did not return")
		}
	}
	waitFor(t, "waiters to drain", func() bool { return l.Waiting() == 0 })
	l.Release()
	if got := l.Active(); got != 0 {
		t.Fatalf("active = %d, want 0", got)
	}
}

// 上限 0 = 完全不限：Acquire/Release/TryAcquire 均为 no-op。
func TestRecordLimiterDisabledIsNoop(t *testing.T) {
	l := NewRecordLimiter(0)
	if l.Enabled() {
		t.Fatal("limiter with max=0 must be disabled")
	}
	for i := 0; i < 100; i++ {
		if err := l.Acquire(context.Background()); err != nil {
			t.Fatalf("acquire #%d: %v", i, err)
		}
	}
	if waiter, ok := l.Reserve(); !ok || waiter != nil {
		t.Fatalf("Reserve on disabled limiter = (%v, %v), want (nil, true)", waiter, ok)
	}
	l.Release()
	l.Release()
	if l.Active() != 0 || l.Waiting() != 0 {
		t.Fatalf("disabled limiter state changed: active=%d waiting=%d", l.Active(), l.Waiting())
	}
}

// gate：启用时先 TryAcquire，槽位占用且队列满时必须返回 Refused，不能静默停录。
func TestGateRecordRefusesWhenQueueFull(t *testing.T) {
	p := &MP4Plugin{}
	p.Logger = slog.New(slog.DiscardHandler)
	l := NewRecordLimiter(1)
	l.timeout = 10 * time.Millisecond
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}
	l.mu.Lock()
	for i := 0; i < l.queueCapacity; i++ {
		l.waiters = append(l.waiters, &recordWaiter{ch: make(chan struct{}, 1), limiter: l})
	}
	l.mu.Unlock()
	p.recordLimiter.Store(l)

	admission := p.GateRecord(&m7s.Publisher{}, config.Record{})
	if admission.Release != nil {
		t.Fatal("refused recording must not return a release fn")
	}
	if admission.Result != m7s.RecordGateRefused {
		t.Fatalf("result = %v, want RecordGateRefused", admission.Result)
	}
}

// gate：上限 0 时必须完全放行（保持现有行为）。
func TestGateRecordDisabledAdmits(t *testing.T) {
	p := &MP4Plugin{}
	p.recordLimiter.Store(NewRecordLimiter(0))
	admission := p.GateRecord(&m7s.Publisher{}, config.Record{})
	if admission.Release != nil {
		t.Fatal("disabled limiter must not return a release fn")
	}
	if admission.Result != m7s.RecordGateAdmit {
		t.Fatalf("result = %v, want RecordGateAdmit", admission.Result)
	}
}

// StartRecord 在 limiter 拒绝时返回 503 映射（codes.Unavailable）。
func TestStartRecordReturnsUnavailableWhenLimiterRefuses(t *testing.T) {
	p := newLimiterTestPlugin(t)
	l := NewRecordLimiter(1)
	l.timeout = 10 * time.Millisecond
	l.active = 1
	l.mu.Lock()
	for i := 0; i < l.queueCapacity; i++ {
		l.waiters = append(l.waiters, &recordWaiter{ch: make(chan struct{}, 1), limiter: l})
	}
	l.mu.Unlock()
	p.recordLimiter.Store(l)

	_, err := p.StartRecord(context.Background(), &mp4pb.ReqStartRecord{StreamPath: "live/test"})
	if err == nil {
		t.Fatal("StartRecord should fail when limiter refuses")
	}
	if got := status.Code(err); got != codes.Unavailable {
		t.Fatalf("status code = %v, want %v (err=%v)", got, codes.Unavailable, err)
	}
}

// StartRecord 在取得名额后失败（此处为流不存在）必须释放名额，不得泄漏。
func TestStartRecordReleasesSlotOnFailure(t *testing.T) {
	p := newLimiterTestPlugin(t)
	l := NewRecordLimiter(1)
	l.timeout = 20 * time.Millisecond
	p.recordLimiter.Store(l)

	_, err := p.StartRecord(context.Background(), &mp4pb.ReqStartRecord{StreamPath: "live/missing"})
	if err == nil {
		t.Fatal("StartRecord should fail for a missing stream")
	}
	if status.Code(err) == codes.Unavailable {
		t.Fatalf("missing stream must not be reported as limiter refusal: %v", err)
	}
	if got := l.Active(); got != 0 {
		t.Fatalf("active = %d after failed StartRecord, want 0 (slot leaked)", got)
	}
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("slot was leaked, re-acquire failed: %v", err)
	}
	l.Release()
}

// 上限 0 时 StartRecord 行为与现状一致（不排队、不拒绝）。
func TestStartRecordUnlimitedPreservesBehavior(t *testing.T) {
	p := newLimiterTestPlugin(t)
	l := NewRecordLimiter(0)
	p.recordLimiter.Store(l)

	_, err := p.StartRecord(context.Background(), &mp4pb.ReqStartRecord{StreamPath: "live/missing"})
	if err == nil {
		t.Fatal("StartRecord should fail for a missing stream")
	}
	if status.Code(err) == codes.Unavailable {
		t.Fatalf("unlimited limiter must not refuse: %v", err)
	}
	if l.Active() != 0 || l.Waiting() != 0 {
		t.Fatalf("disabled limiter state changed: active=%d waiting=%d", l.Active(), l.Waiting())
	}
}

// 事件录像入口必须与 StartRecord 走同一套限流：名额满且队列满时返回 503，
// 而不是绕过 maxconcurrentrecordings 直接开录。
func TestEventStartGoesThroughLimiter(t *testing.T) {
	p := newLimiterTestPlugin(t)
	l := NewRecordLimiter(1)
	l.timeout = 10 * time.Millisecond
	l.active = 1
	l.mu.Lock()
	for i := 0; i < l.queueCapacity; i++ {
		l.waiters = append(l.waiters, &recordWaiter{ch: make(chan struct{}, 1), limiter: l})
	}
	l.mu.Unlock()
	p.recordLimiter.Store(l)

	_, err := p.EventStart(context.Background(), &mp4pb.ReqEventRecord{StreamPath: "live/missing"})
	if err == nil {
		t.Fatal("EventStart must be refused when the limiter is saturated")
	}
	if got := status.Code(err); got != codes.Unavailable {
		t.Fatalf("status code = %v, want %v (err=%v)", got, codes.Unavailable, err)
	}
	if got := l.Waiting(); got != l.queueCapacity {
		t.Fatalf("waiting = %d, want %d (limiter state must be untouched)", got, l.queueCapacity)
	}
}

// RecordGateResult 的零值必须是 RecordGateUnset：调用方据此 fail-closed，
// 未显式给出结论的插件不得被当作“准入”。
func TestRecordGateUnsetIsZeroValue(t *testing.T) {
	var unset m7s.RecordGateResult
	if unset != m7s.RecordGateUnset {
		t.Fatalf("zero value = %v, want RecordGateUnset", unset)
	}
	if m7s.RecordGateAdmit == m7s.RecordGateUnset {
		t.Fatal("RecordGateAdmit must not be the zero value")
	}
	if m7s.RecordGateQueued == m7s.RecordGateUnset || m7s.RecordGateRefused == m7s.RecordGateUnset {
		t.Fatal("all gate results must be distinct from the zero value")
	}
}

func newLimiterTestPlugin(t *testing.T) *MP4Plugin {
	t.Helper()
	p := &MP4Plugin{}
	p.Logger = slog.New(slog.DiscardHandler)
	p.Server = &m7s.Server{}
	return p
}

// ---- 生命周期回归测试 ----
//
// 这些用例钉住 Task 12 修复的关键生命周期：
//   - 录制任务 dispose 时名额恰好释放一次；
//   - Queued 下载器在 Release 后取得名额并真正启动录制；
//   - 启动失败（Records 已停止）时立即归还名额。

// fakeRecordingRecorder 是极简 IRecorder：Start 只发信号，不做真实 IO。
type fakeRecordingRecorder struct {
	task.Task
	job     m7s.RecordJob
	started chan struct{}
}

func (r *fakeRecordingRecorder) GetRecordJob() *m7s.RecordJob { return &r.job }

func (r *fakeRecordingRecorder) Start() error {
	if r.started != nil {
		select {
		case r.started <- struct{}{}:
		default:
		}
	}
	return nil
}

type recordingTestEnv struct {
	plugin *MP4Plugin
	server *m7s.Server
	rec    *fakeRecordingRecorder
	pub    *m7s.Publisher
}

// newRecordingTestEnv 启动任务系统并构造一个用 fake recorder 的 MP4 插件。
func newRecordingTestEnv(t *testing.T) *recordingTestEnv {
	t.Helper()
	var recordRoot task.RootManager[uint32, task.ManagerItem[uint32]]
	recordRoot.Init()
	// 不注册 root.Stop 清理：gotask v1.0.5 在 Work 的最后一个子任务 dispose 时
	// 会于旧 event loop 退出的同时再起一个（event_loop.go:141/146 已知数据竞争），
	// 停止 root 必然触发。测试进程结束即回收，避免把上游缺陷引入本包 -race 门禁。

	rec := &fakeRecordingRecorder{started: make(chan struct{}, 1)}
	s := &m7s.Server{}
	p := &MP4Plugin{}
	p.Logger = slog.New(slog.DiscardHandler)
	p.Server = s
	p.Meta = &m7s.PluginMeta{
		Name:        "MP4",
		NewRecorder: func(config.Record) m7s.IRecorder { return rec },
	}

	if err := recordRoot.AddTask(&s.Records).WaitStarted(); err != nil {
		t.Fatalf("start Records: %v", err)
	}

	// 只需要一个未停止的 Publisher（pendingAutoRecordTask 会检查 IsStopped）；
	// 不启动真实 Publisher，避免触发网络/别名等与限流无关的初始化。
	pub := &m7s.Publisher{}
	pub.StreamPath = "live/queued"
	pub.Context = context.Background()
	return &recordingTestEnv{plugin: p, server: s, rec: rec, pub: pub}
}

// 录制任务 dispose 时，dispose 回调必须且只能执行一次（不得重复归还名额）。
func TestRecordJobDisposeReleasesSlotExactlyOnce(t *testing.T) {
	env := newRecordingTestEnv(t)
	l := NewRecordLimiter(1)
	l.timeout = time.Second
	env.plugin.recordLimiter.Store(l)

	release, err := env.plugin.acquireRecordingSlot(context.Background())
	if err != nil {
		t.Fatalf("acquire slot: %v", err)
	}
	if release == nil {
		t.Fatal("enabled limiter must return a release func")
	}
	if got := l.Active(); got != 1 {
		t.Fatalf("active = %d after acquire, want 1", got)
	}

	var calls atomic.Int32
	counted := func() {
		calls.Add(1)
		release()
	}
	job := env.plugin.RecordWithDispose(env.pub, config.Record{FilePath: "dispose-once.mp4"}, nil, counted)
	if err := job.WaitStarted(); err != nil {
		t.Fatalf("record job failed to start: %v", err)
	}
	// 等 recorder 子任务真正启动：保证 job 自己的 event loop 正在运行，
	// dispose 时 waitChildrenDispose 不会在旧 loop 退出的同时再起一个
	// （gotask v1.0.5 已知 EventLoop 数据竞争）。
	select {
	case <-env.rec.started:
	case <-time.After(time.Second):
		t.Fatal("recorder child did not start")
	}
	// 再挂一个常驻空任务，确保 dispose 期间 job 的 event loop 一定处于运行态：
	// waitChildrenDispose 的 active() 不会在旧 loop 退出时再起一个新 loop
	// （gotask v1.0.5 event_loop.go:141/146 已知数据竞争）。
	var keeper task.Task
	if err := job.AddTask(&keeper).WaitStarted(); err != nil {
		t.Fatalf("add keeper task: %v", err)
	}
	if got := l.Active(); got != 1 {
		t.Fatalf("active = %d while recording, want 1", got)
	}

	job.Stop(task.ErrTaskComplete)
	if err := job.WaitStopped(); err != nil && !errors.Is(err, task.ErrTaskComplete) {
		t.Fatalf("wait job stopped: %v", err)
	}

	waitFor(t, "dispose release", func() bool { return calls.Load() == 1 })
	time.Sleep(20 * time.Millisecond) // 给迟到的第二次回调留出暴露窗口
	if got := calls.Load(); got != 1 {
		t.Fatalf("release called %d times, want exactly 1", got)
	}
	if got := l.Active(); got != 0 {
		t.Fatalf("active = %d after dispose, want 0", got)
	}
	// 让 Records 的 event loop 完全退出后再执行 cleanup，规避 gotask v1.0.5
	// 已知的 EventLoop.run 退出/defer 数据竞争（CLAUDE.md Known Issues）。
	waitFor(t, "Records event loop to exit", func() bool { return !env.server.Records.EventLoopRunning() })
	time.Sleep(20 * time.Millisecond)
}

// 排队任务必须**自己**申请名额（GateRecord 不再预占）：名额被占满时先排队，
// Release 后取得名额并启动录制。
func TestPendingAutoRecordQueuedThenStarts(t *testing.T) {
	env := newRecordingTestEnv(t)
	l := NewRecordLimiter(1)
	l.timeout = time.Second
	env.plugin.recordLimiter.Store(l)
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}

	todo := &pendingAutoRecordTask{
		plugin: env.plugin,
		pub:    env.pub,
		conf:   config.Record{FilePath: "queued.mp4"},
		key:    "queued-record-test",
	}
	todo.Logger = slog.New(slog.DiscardHandler)
	todo.Context = context.Background() // 生产环境由 AddTask 注入；直接调 Go() 时需要

	done := make(chan error, 1)
	go func() { done <- todo.Go() }()

	waitFor(t, "pending task to queue", func() bool { return l.Waiting() == 1 })
	select {
	case <-env.rec.started:
		t.Fatal("recording started before a slot was released")
	default:
	}

	l.Release()
	select {
	case <-env.rec.started:
	case <-time.After(time.Second):
		t.Fatal("queued recording did not start after slot release")
	}
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, task.ErrTaskComplete) {
			t.Fatalf("pending task Go: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending task Go did not return")
	}

	if got := l.Active(); got != 1 {
		t.Fatalf("active while recording = %d, want 1", got)
	}
	stopQueuedRecording(t, env, l)
}

// 启动失败（此处 Records 已停止，AddTask 被拒）必须立即归还名额。
func TestPendingAutoRecordReleasesSlotWhenStartFails(t *testing.T) {
	env := newRecordingTestEnv(t)
	l := NewRecordLimiter(1)
	l.timeout = time.Second
	env.plugin.recordLimiter.Store(l)
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}

	// Records 停止后，RecordJob 的 AddTask 必然被拒 → WaitStarted 失败。
	env.server.Records.Stop(task.ErrStopByUser)
	if err := env.server.Records.WaitStopped(); err != nil && !errors.Is(err, task.ErrStopByUser) {
		t.Fatalf("stop Records: %v", err)
	}

	todo := &pendingAutoRecordTask{
		plugin: env.plugin,
		pub:    env.pub,
		conf:   config.Record{FilePath: "start-fails.mp4"},
		key:    "failed-record-test",
	}
	todo.Logger = slog.New(slog.DiscardHandler)
	todo.Context = context.Background() // 生产环境由 AddTask 注入；直接调 Go() 时需要

	done := make(chan error, 1)
	go func() { done <- todo.Go() }()
	waitFor(t, "pending task to queue", func() bool { return l.Waiting() == 1 })

	l.Release()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, task.ErrTaskComplete) {
			t.Fatalf("pending task Go: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending task Go did not return")
	}

	if got := l.Active(); got != 0 {
		t.Fatalf("active = %d after start failure, want 0 (slot leaked)", got)
	}
	select {
	case <-env.rec.started:
		t.Fatal("recorder must not start when RecordJob startup fails")
	default:
	}
}

// I1：waiter 属于另一个 limiter 代次时必须视为已取消，且不得在错误的 limiter
// 上释放名额（旧实现会把 removeWaiter=false 当作“名额已交接”而误释放）。
func TestRecordLimiterWaitRejectsForeignWaiter(t *testing.T) {
	oldLimiter := NewRecordLimiter(1)
	oldLimiter.timeout = 10 * time.Millisecond
	if err := oldLimiter.Acquire(context.Background()); err != nil {
		t.Fatalf("old limiter acquire: %v", err)
	}
	waiter, ok := oldLimiter.Reserve()
	if !ok || waiter == nil {
		t.Fatalf("Reserve = (%v, %v), want a queued waiter", waiter, ok)
	}

	newLimiter := NewRecordLimiter(1)
	newLimiter.timeout = 10 * time.Millisecond

	start := time.Now()
	err := newLimiter.Wait(context.Background(), waiter)
	if !errors.Is(err, ErrRecordLimiterStale) {
		t.Fatalf("Wait on foreign limiter = %v, want ErrRecordLimiterStale", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("foreign waiter should be rejected immediately, took %v", elapsed)
	}
	if got := newLimiter.Active(); got != 0 {
		t.Fatalf("new limiter active = %d, want 0", got)
	}
	if got := newLimiter.Waiting(); got != 0 {
		t.Fatalf("new limiter waiting = %d, want 0", got)
	}

	// 清理旧 limiter：名额交接给旧 waiter 后，再归还一次。
	oldLimiter.Release()
	if err := oldLimiter.Wait(context.Background(), waiter); err != nil {
		t.Fatalf("old waiter should receive handover: %v", err)
	}
	oldLimiter.Release()
	if got := oldLimiter.Active(); got != 0 {
		t.Fatalf("old limiter active = %d after cleanup, want 0", got)
	}
}

// 并发安全：多 goroutine 同时 Acquire/Release 后名额必须归零（配合 -race）。
func TestRecordLimiterConcurrentAcquireRelease(t *testing.T) {
	l := NewRecordLimiter(4)
	l.timeout = time.Second
	const workers = 32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := l.Acquire(ctx); err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			l.Release()
		}()
	}
	wg.Wait()
	if got := l.Active(); got != 0 {
		t.Fatalf("active = %d after all releases, want 0", got)
	}
	if got := l.Waiting(); got != 0 {
		t.Fatalf("waiting = %d after all releases, want 0", got)
	}
}
