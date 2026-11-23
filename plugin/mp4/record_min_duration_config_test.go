package plugin_mp4

import (
	"testing"
	"time"

	mp4pkg "m7s.live/v5/plugin/mp4/pkg"
)

// record.minduration 必须真正注入到 pkg 层录制器：录制器在 pkg 包内，插件配置
// 不注入就等于配置被静默忽略（丢弃功能完全不生效）。
func TestApplyRecordMinDurationConfig(t *testing.T) {
	prev := mp4pkg.GetRecordConfig()
	t.Cleanup(func() { mp4pkg.SetRecordConfig(prev) })

	p := &MP4Plugin{}
	p.RecordConfig.MinDuration = 3 * time.Second

	p.applyRecordMinDurationConfig()

	if got := mp4pkg.GetRecordConfig().MinDuration; got != 3*time.Second {
		t.Fatalf("record.minduration = %v, want 3s", got)
	}
}

// 未配置时必须是 0（不丢弃），保持既有行为。
func TestApplyRecordMinDurationDefaultsToZero(t *testing.T) {
	prev := mp4pkg.GetRecordConfig()
	t.Cleanup(func() { mp4pkg.SetRecordConfig(prev) })
	mp4pkg.SetRecordConfig(mp4pkg.RecordConfig{MinDuration: time.Second})

	p := &MP4Plugin{}
	p.applyRecordMinDurationConfig()

	if got := mp4pkg.GetRecordConfig().MinDuration; got != 0 {
		t.Fatalf("default record.minduration = %v, want 0 (disabled)", got)
	}
}
