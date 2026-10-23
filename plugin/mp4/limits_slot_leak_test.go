package plugin_mp4

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	task "github.com/eanfs/gotask"
	m7s "m7s.live/v5"
	"m7s.live/v5/pkg/config"
)

// 这组用例钉住 C2：名额必须 exactly-once，任何 AddTask 拒绝路径都不得留下孤儿
// waiter 或永久占用的名额（旧实现 GateRecord 先 Reserve 再 AddTask，AddTask 被
// 同步拒绝后 waiter 常驻，每触发一次永久少一个名额）。

// newGateTestEnv 在 newRecordingTestEnv 之上把 MP4 插件本身作为任务启动：
// GateRecord 的排队路径要把它自己的待录任务 AddTask 到已启动的 Job 上。
func newGateTestEnv(t *testing.T) *recordingTestEnv {
	t.Helper()
	return newGateTestEnvWithLogger(t, slog.New(slog.DiscardHandler))
}

// newGateTestEnvWithLogger 同上，但允许在插件启动前替换 Logger（启动后再改
// plugin.Logger 会与事件循环里的 Task.Debug/Error 读写同一字段，-race 报错）。
func newGateTestEnvWithLogger(t *testing.T, logger *slog.Logger) *recordingTestEnv {
	t.Helper()
	env := newRecordingTestEnv(t)
	env.plugin.Logger = logger
	var pluginRoot task.RootManager[uint32, task.ManagerItem[uint32]]
	pluginRoot.Init()
	// 不注册 Stop 清理：gotask v1.0.5 在 Work 的最后一个子任务 dispose 时存在
	// 已知的 EventLoop 数据竞争（event_loop.go:141/146），停止 root 会触发。
	if err := pluginRoot.AddTask(env.plugin).WaitStarted(); err != nil {
		t.Fatalf("start mp4 plugin: %v", err)
	}
	return env
}

// stopQueuedRecording 停掉排队路径启动的录制任务，验证名额随 dispose 归还。
func stopQueuedRecording(t *testing.T, env *recordingTestEnv, l *RecordLimiter) {
	t.Helper()
	// 常驻空任务保证 dispose 期间 event loop 处于运行态（gotask v1.0.5 已知
	// EventLoop 数据竞争，见 TestRecordJobDisposeReleasesSlotExactlyOnce）。
	var keeper task.Task
	if err := env.rec.job.AddTask(&keeper).WaitStarted(); err != nil {
		t.Fatalf("add keeper task: %v", err)
	}
	env.rec.job.GetTask().Stop(task.ErrTaskComplete)
	waitFor(t, "queued recording to release slot", func() bool { return l.Active() == 0 })
	waitFor(t, "Records event loop to exit", func() bool { return !env.server.Records.EventLoopRunning() })
	time.Sleep(20 * time.Millisecond)
}

// GateRecord 在启用限流时必须只排队、不预占名额：名额由待录任务在 Go() 中自行
// 申请，这样 AddTask 被拒绝时不可能遗留 waiter/名额。
func TestGateRecordQueuesWithoutReservingSlot(t *testing.T) {
	env := newGateTestEnv(t)
	l := NewRecordLimiter(1)
	l.timeout = 5 * time.Second
	env.plugin.recordLimiter.Store(l)

	admission := env.plugin.GateRecord(env.pub, config.Record{FilePath: "gate-queued.mp4"})
	if admission.Release != nil {
		t.Fatal("queued gate result must not carry a release fn")
	}
	if admission.Result != m7s.RecordGateQueued {
		t.Fatalf("result = %v, want RecordGateQueued (GateRecord must not reserve on the publish path)", admission.Result)
	}

	// 排队任务自行取得名额后必须真正启动录制，且名额恰好被占用一次。
	select {
	case <-env.rec.started:
	case <-time.After(time.Second):
		t.Fatal("queued gate recording did not start")
	}
	if got := l.Active(); got != 1 {
		t.Fatalf("active = %d while recording, want 1", got)
	}
	stopQueuedRecording(t, env, l)
}

// stoppedTestPlugin 构造一个“已停止”的插件：根上下文已取消，AddTask 在
// initContext 之后立即被同步拒绝，不需要（也不能）真正走任务生命周期——
// Stop 一个带子任务的 Job 会触发 gotask v1.0.5 已知的 EventLoop dispose 竞态，
// 那与本用例要钉的限流逻辑无关。
func stoppedTestPlugin(t *testing.T) *MP4Plugin {
	t.Helper()
	p := &MP4Plugin{}
	p.Logger = slog.New(slog.DiscardHandler)
	p.Server = &m7s.Server{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Context = ctx
	return p
}

// 插件已停止时 queueAutoRecording 的 AddTask 必定被同步拒绝：必须返回 false
// （GateRecord 据此返回 Refused），并且不留下任何 waiter 或名额。
//
// 覆盖边界（M-5）：本用例走的是 AddTaskRejected 的 `parent.IsStopped()` 平凡
// 分支；“键冲突 / 事件循环输入通道满”那条非平凡分支（上下文仍有效但任务未登记）
// 需要灌满 gotask 的输入通道，无法确定性复现，由 root 包的
// TestAddTaskRejectedDetectsUnregisteredTask 直接钉住识别逻辑。
func TestQueueAutoRecordingRejectedLeavesNoWaiter(t *testing.T) {
	p := stoppedTestPlugin(t)
	l := NewRecordLimiter(1)
	l.timeout = time.Second
	p.recordLimiter.Store(l)

	pub := &m7s.Publisher{}
	pub.StreamPath = "live/rejected"

	for i := 0; i < 10; i++ {
		if p.queueAutoRecording(pub, config.Record{FilePath: "rejected.mp4"}, l) {
			t.Fatalf("queueAutoRecording #%d = true, want false (AddTask must be rejected)", i)
		}
	}
	if got := l.Waiting(); got != 0 {
		t.Fatalf("waiting = %d after rejected AddTask, want 0 (orphan waiter)", got)
	}
	if got := l.Active(); got != 0 {
		t.Fatalf("active = %d after rejected AddTask, want 0", got)
	}
	// 连续拒绝后名额仍然可用：不泄漏。
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("slot leaked after repeated rejections: %v", err)
	}
	l.Release()
}

// 插件已停止时 GateRecord 必须返回 Refused 且不留 waiter；重复调用不影响名额。
func TestGateRecordRefusedLeavesNoWaiter(t *testing.T) {
	p := stoppedTestPlugin(t)
	l := NewRecordLimiter(1)
	l.timeout = time.Second
	p.recordLimiter.Store(l)

	pub := &m7s.Publisher{}
	pub.StreamPath = "live/refused"

	for i := 0; i < 10; i++ {
		admission := p.GateRecord(pub, config.Record{FilePath: "refused.mp4"})
		if admission.Release != nil {
			t.Fatal("refused result must not carry a release fn")
		}
		if admission.Result != m7s.RecordGateRefused {
			t.Fatalf("result #%d = %v, want RecordGateRefused", i, admission.Result)
		}
	}
	if got := l.Waiting(); got != 0 {
		t.Fatalf("waiting = %d, want 0", got)
	}
	if got := l.Active(); got != 0 {
		t.Fatalf("active = %d, want 0", got)
	}
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("slot leaked after repeated refusals: %v", err)
	}
	l.Release()
}

// publisher 在排队期间已停止：不得启动录制（pub.Using 的并发窗口），名额必须归还。
//
// 断言刻意区分“取得名额后又归还”与“从未取得”：先把唯一名额占满，等任务进入
// Acquire 的等待队列（Waiting()==1 证明它走到了排队路径），再 Release 把名额
// 交接给它——若它没有在发现 publisher 已停止后归还，Active() 会停在 1。
func TestPendingAutoRecordSkipsStoppedPublisherAndReleasesSlot(t *testing.T) {
	env := newRecordingTestEnv(t)
	l := NewRecordLimiter(1)
	l.timeout = time.Second
	env.plugin.recordLimiter.Store(l)
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}

	stoppedPub := &m7s.Publisher{}
	stoppedPub.StreamPath = "live/stopped"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stoppedPub.Context = ctx

	todo := &pendingAutoRecordTask{
		plugin: env.plugin,
		pub:    stoppedPub,
		conf:   config.Record{FilePath: "stopped-pub.mp4"},
		key:    "stopped-publisher-test",
	}
	todo.Logger = slog.New(slog.DiscardHandler)
	todo.Context = context.Background()

	done := make(chan error, 1)
	go func() { done <- todo.Go() }()
	waitFor(t, "pending task to queue", func() bool { return l.Waiting() == 1 })

	l.Release() // 名额交接给排队任务（Active 保持 1）
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, task.ErrTaskComplete) {
			t.Fatalf("pending task Go: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending task Go did not return")
	}

	// 任务确实持有过名额（交接）→ 必须由它自己归还。
	waitFor(t, "slot released after publisher stop", func() bool { return l.Active() == 0 })
	if got := l.Waiting(); got != 0 {
		t.Fatalf("waiting = %d, want 0", got)
	}
	select {
	case <-env.rec.started:
		t.Fatal("recording must not start when the publisher is already stopped")
	default:
	}
}

// logRecorder 记录所有 slog 记录，用于断言日志级别（I-2：排队日志不是饱和信号）。
type logRecorder struct {
	mu    sync.Mutex
	items []slog.Record
}

func (h *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (h *logRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.items = append(h.items, r.Clone())
	return nil
}

func (h *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *logRecorder) WithGroup(string) slog.Handler      { return h }

func (h *logRecorder) count(level slog.Level, msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.items {
		if r.Level == level && r.Message == msg {
			n++
		}
	}
	return n
}

// 排队日志分级：有空闲名额时只打 Debug（每路自动录像都会排队，Info 会被误读成
// 饱和），只有真正需要等待（名额已满或已有等待者）才打 Info 并带 active/waiting。
func TestQueueAutoRecordingLogsInfoOnlyWhenSaturated(t *testing.T) {
	logs := &logRecorder{}
	env := newGateTestEnvWithLogger(t, slog.New(logs))
	l := NewRecordLimiter(1)
	l.timeout = 50 * time.Millisecond // 第二个排队任务快速超时，避免占用整个测试
	env.plugin.recordLimiter.Store(l)

	// (1) 名额空闲：只能有 Debug。
	admission := env.plugin.GateRecord(env.pub, config.Record{FilePath: "free-slot.mp4"})
	if admission.Result != m7s.RecordGateQueued || admission.Release != nil {
		t.Fatalf("GateRecord = (%v, %v), want (nil, Queued)", admission.Release != nil, admission.Result)
	}
	if got := logs.count(slog.LevelInfo, "auto recording queued by concurrency limit"); got != 0 {
		t.Fatalf("info queued log = %d with a free slot, want 0 (noise)", got)
	}
	if got := logs.count(slog.LevelDebug, "auto recording queued"); got != 1 {
		t.Fatalf("debug queued log = %d with a free slot, want 1", got)
	}
	select {
	case <-env.rec.started:
	case <-time.After(time.Second):
		t.Fatal("recording with a free slot did not start")
	}

	// (2) 名额已满（上一步的录制占着唯一名额）：Info + active/waiting 字段。
	if admission = env.plugin.GateRecord(env.pub, config.Record{FilePath: "waiting.mp4"}); admission.Result != m7s.RecordGateQueued || admission.Release != nil {
		t.Fatalf("GateRecord = (%v, %v), want (nil, Queued)", admission.Release != nil, admission.Result)
	}
	if got := logs.count(slog.LevelInfo, "auto recording queued by concurrency limit"); got != 1 {
		t.Fatalf("info queued log = %d when saturated, want 1", got)
	}
	// 等第二个排队任务真正超时退出（它会在 Acquire 失败时记 Error）：只等
	// Waiting()==0 会在它还没入队时就成立，随后 Release 会把名额交接给它，
	// 于是又启动一路录制（共享 fake job 会被重新 Init），cleanup 永远等不到 0。
	waitFor(t, "second pending task to time out", func() bool {
		return logs.count(slog.LevelError, "auto recording refused by recording concurrency limit") == 1
	})
	waitFor(t, "waiter drained", func() bool { return l.Waiting() == 0 })

	stopQueuedRecording(t, env, l)
}

// I-1：publisher 在注册窗口内停止时，“注册后复核”必须 Stop 掉 RecordJob，
// 由 dispose 回调归还名额。否则该 job 既不在 publisher 的 resources 里（没人
// stop），recorder 又是无限重试（SetRetry(-1,…)），孤儿任务会永久占住名额。
func TestRecordWithDisposeStopsJobWhenPublisherAlreadyStopped(t *testing.T) {
	if raceEnabled {
		// 本用例必然“停下刚创建的 RecordJob”：gotask v1.0.5 里 Job 的 dispose
		// （waitChildrenDispose → 子任务 Stop，读 Task.StartTime）会与 recorder
		// 子任务的 start()（写 Task.StartTime）并发，-race 下必报数据竞争。
		// 这是上游缺陷（CLAUDE.md Known Issues 同类），与被测的限流逻辑无关；
		// 用例在非 -race 门禁下运行，覆盖“注册后复核会 Stop”这一分支。
		t.Skip("skipping under -race: gotask v1.0.5 Stop/start race on a freshly created Job")
	}
	env := newRecordingTestEnv(t)
	l := NewRecordLimiter(1)
	l.timeout = time.Second
	env.plugin.recordLimiter.Store(l)

	release, err := env.plugin.acquireRecordingSlot(context.Background())
	if err != nil {
		t.Fatalf("acquire slot: %v", err)
	}
	var calls atomic.Int32
	counted := func() {
		calls.Add(1)
		release()
	}

	stoppedPub := &m7s.Publisher{}
	stoppedPub.StreamPath = "live/gone"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stoppedPub.Context = ctx

	job := env.plugin.RecordWithDispose(stoppedPub, config.Record{FilePath: "orphan.mp4"}, nil, counted)
	waitFor(t, "recording job disposed", func() bool { return job.GetTask().IsStopped() })
	waitFor(t, "slot released", func() bool { return l.Active() == 0 })
	waitFor(t, "release callback", func() bool { return calls.Load() == 1 })
	// 幂等性：dispose 结束后不得再有一次迟到的释放。
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("release called %d times, want exactly 1", got)
	}
	waitFor(t, "Records event loop to exit", func() bool { return !env.server.Records.EventLoopRunning() })
	time.Sleep(20 * time.Millisecond)
}
