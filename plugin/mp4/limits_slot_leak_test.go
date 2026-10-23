package plugin_mp4

import (
	"context"
	"errors"
	"log/slog"
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
	env := newRecordingTestEnv(t)
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

	release, result := env.plugin.GateRecord(env.pub, config.Record{FilePath: "gate-queued.mp4"})
	if release != nil {
		t.Fatal("queued gate result must not carry a release fn")
	}
	if result != m7s.RecordGateQueued {
		t.Fatalf("result = %v, want RecordGateQueued (GateRecord must not reserve on the publish path)", result)
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
func TestQueueAutoRecordingRejectedLeavesNoWaiter(t *testing.T) {
	p := stoppedTestPlugin(t)
	l := NewRecordLimiter(1)
	l.timeout = time.Second
	p.recordLimiter.Store(l)

	pub := &m7s.Publisher{}
	pub.StreamPath = "live/rejected"

	for i := 0; i < 10; i++ {
		if p.queueAutoRecording(pub, config.Record{FilePath: "rejected.mp4"}) {
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
		release, result := p.GateRecord(pub, config.Record{FilePath: "refused.mp4"})
		if release != nil {
			t.Fatal("refused result must not carry a release fn")
		}
		if result != m7s.RecordGateRefused {
			t.Fatalf("result #%d = %v, want RecordGateRefused", i, result)
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
func TestPendingAutoRecordSkipsStoppedPublisherAndReleasesSlot(t *testing.T) {
	env := newRecordingTestEnv(t)
	l := NewRecordLimiter(1)
	l.timeout = time.Second
	env.plugin.recordLimiter.Store(l)

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
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, task.ErrTaskComplete) {
			t.Fatalf("pending task Go: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending task Go did not return")
	}

	if got := l.Active(); got != 0 {
		t.Fatalf("active = %d after publisher stop, want 0 (slot leaked)", got)
	}
	select {
	case <-env.rec.started:
		t.Fatal("recording must not start when the publisher is already stopped")
	default:
	}
}
