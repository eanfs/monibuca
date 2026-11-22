package plugin_mp4

import (
	"context"
	"sync"
	"testing"
	"time"

	"m7s.live/v5/pkg/config"
	mp4pb "m7s.live/v5/plugin/mp4/pb"
)

// newResumeWiringEnv 在 newRecordingTestEnv 基础上补齐 StartRecord/续录所需的
// 最小接线：意图开启、存储校验放行、Streams 可被 SafeGet 命中。
func newResumeWiringEnv(t *testing.T) *recordingTestEnv {
	t.Helper()
	env := newRecordingTestEnv(t)
	p := env.plugin
	p.Context = context.Background()
	p.RecordConfig.AutoResume.Enable = true
	p.storageValidator = func() error { return nil }
	// 不启动 Streams 事件循环：给 Collection 一把锁并直接塞入 publisher，
	// SafeGet 走加锁分支，避免测试依赖 Streams 的 Work 生命周期。
	env.server.Streams.L = &sync.RWMutex{}
	env.server.Streams.Collection.Add(env.pub)
	return env
}

// TestStartStopRecordWiresResumeIntent 钉住 API 调用点：
// StartRecord 成功后必须登记意图，StopRecord 必须删除意图。
//
// 只测 registerResumeIntent/forgetResumeIntent 本身是摆设——删掉 api.go 里的
// 两处调用，那些用例仍然全绿。
func TestStartStopRecordWiresResumeIntent(t *testing.T) {
	env := newResumeWiringEnv(t)
	ctx := context.Background()

	res, err := env.plugin.StartRecord(ctx, &mp4pb.ReqStartRecord{
		StreamPath: env.pub.StreamPath,
		Duration:   "1m",
		Fragment:   "0",
	})
	if err != nil {
		t.Fatalf("StartRecord: %v", err)
	}
	if res.Data == 0 {
		t.Fatal("StartRecord returned zero task pointer")
	}
	snap := env.plugin.resumeStore.snapshot()
	if len(snap) != 1 || snap[0].streamPath != env.pub.StreamPath {
		t.Fatalf("resume intent not registered by StartRecord: %+v", snap)
	}

	if _, err := env.plugin.StopRecord(ctx, &mp4pb.ReqStopRecord{StreamPath: env.pub.StreamPath}); err != nil {
		t.Fatalf("StopRecord: %v", err)
	}
	if n := len(env.plugin.resumeStore.snapshot()); n != 0 {
		t.Fatalf("resume intent left after StopRecord: %d", n)
	}
}

// TestStartRecordWithoutDurationDoesNotRegisterIntent 钉住登记规则在调用点的生效：
// duration=0（无限录制）不登记，即便 autoResume 已开启。
func TestStartRecordWithoutDurationDoesNotRegisterIntent(t *testing.T) {
	env := newResumeWiringEnv(t)
	if _, err := env.plugin.StartRecord(context.Background(), &mp4pb.ReqStartRecord{
		StreamPath: env.pub.StreamPath,
		Fragment:   "0",
	}); err != nil {
		t.Fatalf("StartRecord: %v", err)
	}
	if n := len(env.plugin.resumeStore.snapshot()); n != 0 {
		t.Fatalf("intent registered for duration=0: %d", n)
	}
}

// TestStartRecordIsIdempotentForRunningRecord 钉住默认开启自动续录后的兼容行为：
// 业务补偿对同一目标再次 start 时必须返回 200 + 已有 task，而不是 409；且不得
// 重置意图 deadline（否则累计时长会超过原请求）。
func TestStartRecordIsIdempotentForRunningRecord(t *testing.T) {
	env := newResumeWiringEnv(t)
	ctx := context.Background()
	req := &mp4pb.ReqStartRecord{
		StreamPath: env.pub.StreamPath,
		Duration:   "1m",
		Fragment:   "0",
	}
	first, err := env.plugin.StartRecord(ctx, req)
	if err != nil {
		t.Fatalf("first StartRecord: %v", err)
	}
	snap := env.plugin.resumeStore.snapshot()
	if len(snap) != 1 {
		t.Fatalf("intent count = %d, want 1", len(snap))
	}
	deadline := snap[0].deadline

	second, err := env.plugin.StartRecord(ctx, req)
	if err != nil {
		t.Fatalf("duplicate StartRecord = %v, want idempotent success", err)
	}
	if second.Data != first.Data {
		t.Fatalf("duplicate StartRecord task = %d, want existing %d", second.Data, first.Data)
	}
	snap = env.plugin.resumeStore.snapshot()
	if len(snap) != 1 || !snap[0].deadline.Equal(deadline) {
		t.Fatalf("duplicate StartRecord re-registered intent: %+v", snap)
	}
}

// TestResumeRecordingDropsFixedFileNameAndUsesRemainingDuration 钉住续录的两个
// 硬约束：固定 fileName 必须丢弃（否则覆盖上一段），Duration 必须取 deadline 剩余量。
func TestResumeRecordingDropsFixedFileNameAndUsesRemainingDuration(t *testing.T) {
	env := newResumeWiringEnv(t)
	it := &resumeIntent{
		streamPath: env.pub.StreamPath,
		conf:       config.Record{FilePath: ".", FileName: "fixed.mp4"},
		deadline:   time.Now().Add(10 * time.Minute),
		segments:   1,
	}
	env.plugin.resumeStore.put(it)

	env.plugin.resumeRecording(it)

	if got := env.rec.job.RecConf.FileName; got != "" {
		t.Fatalf("resumed job kept fixed fileName %q, want empty (must not overwrite)", got)
	}
	if d := env.rec.job.RecConf.Duration; d < 9*time.Minute || d > 10*time.Minute {
		t.Fatalf("resumed duration = %v, want remaining ~10m", d)
	}
	if it.segments != 2 {
		t.Fatalf("segments = %d, want 2", it.segments)
	}
}

// TestResumeRecordingStopsJobWhenIntentRemovedDuringStart 钉住启动后的 isCurrent
// 补检：意图在启动期间被 /mp4/api/stop 删除时，刚拉起的 job 必须被停掉，且不得计段。
func TestResumeRecordingStopsJobWhenIntentRemovedDuringStart(t *testing.T) {
	env := newResumeWiringEnv(t)
	// 不 put：模拟 stop 在 WaitStarted 期间删除意图。
	it := &resumeIntent{
		streamPath: env.pub.StreamPath,
		conf:       config.Record{FilePath: "."},
		deadline:   time.Now().Add(10 * time.Minute),
		segments:   1,
	}

	env.plugin.resumeRecording(it)

	if it.segments != 1 {
		t.Fatalf("segments = %d, want 1 (abandoned segment must not count)", it.segments)
	}
	if !env.rec.job.GetTask().IsStopped() {
		t.Fatal("job from removed intent must be stopped")
	}
}

// TestStopRecordCancelsPendingIntentWithoutJob 钉住单机场景的 Important 修复：
// 录制任务已因断流结束、只剩自动续录意图时，stop 必须按成功返回并清除意图，
// 否则调用方拿到 404 却看到流恢复后录制又被拉起。
func TestStopRecordCancelsPendingIntentWithoutJob(t *testing.T) {
	env := newResumeWiringEnv(t)
	env.plugin.registerResumeIntent(env.pub.StreamPath, config.Record{FilePath: ".", Duration: time.Minute})

	if _, err := env.plugin.StopRecord(context.Background(),
		&mp4pb.ReqStopRecord{StreamPath: env.pub.StreamPath}); err != nil {
		t.Fatalf("StopRecord with pending intent = %v, want success", err)
	}
	if n := len(env.plugin.resumeStore.snapshot()); n != 0 {
		t.Fatalf("pending intent not cleared by StopRecord: %d", n)
	}
}

// TestOnRecordTaskStoppedClearsIntent 钉住通用任务接口的插件钩子：
// /api/task/stop 绕过 /mp4/api/stop，必须仍能清掉自动续录意图。
func TestOnRecordTaskStoppedClearsIntent(t *testing.T) {
	p := newAutoResumeTestPlugin()
	p.RecordConfig.AutoResume.Enable = true
	p.registerResumeIntent("live/a", config.Record{FilePath: ".", Duration: time.Minute})

	p.OnRecordTaskStopped("live/a")

	if n := len(p.resumeStore.snapshot()); n != 0 {
		t.Fatalf("intent not cleared by OnRecordTaskStopped: %d", n)
	}
}
