package m7s

import "testing"

// recordStopHookFakePlugin 记录插件层 stop 钩子的调用。
type recordStopHookFakePlugin struct {
	Plugin
	got []string
}

func (p *recordStopHookFakePlugin) OnRecordTaskStopped(streamPath string) {
	p.got = append(p.got, streamPath)
}

// 通用任务接口 /api/task/stop 直接 Stop RecordJob；插件层状态（mp4 自动续录意图）
// 必须通过钩子清理，否则看护任务会把用户显式停掉的录制重新拉起。
func TestNotifyRecordTaskStoppedDispatchesToHookPlugin(t *testing.T) {
	var fake recordStopHookFakePlugin
	fake.handler = &fake

	s := &Server{}
	s.Plugins.Add(&fake.Plugin)
	s.notifyRecordTaskStopped("live/a")

	if len(fake.got) != 1 || fake.got[0] != "live/a" {
		t.Fatalf("hook calls = %v, want [live/a]", fake.got)
	}
}

// 没有实现钩子的插件不得被调用（类型断言失败时静默跳过）。
func TestNotifyRecordTaskStoppedSkipsPlainPlugin(t *testing.T) {
	var plain Plugin
	plain.handler = &plain

	s := &Server{}
	s.Plugins.Add(&plain)
	// 不应 panic；没有可观察的副作用即为通过。
	s.notifyRecordTaskStopped("live/a")
}
