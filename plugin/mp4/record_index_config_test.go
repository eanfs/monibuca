package plugin_mp4

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	mp4pkg "m7s.live/v5/plugin/mp4/pkg"
)

// 关闭索引不会报错，但会让崩溃恢复彻底失效（没有 sidecar 就没有恢复线索）。
// 这种“合法的静默降级”必须在插件层用插件自己的 logger 告警，而不是在 pkg 层
// 用 slog.Default()（那会绕过插件的日志路由）。
func TestRecordIndexDisabledWarnsAboutDisabledRecovery(t *testing.T) {
	prev := mp4pkg.GetIndexConfig()
	t.Cleanup(func() { mp4pkg.SetIndexConfig(prev) })

	var logs bytes.Buffer
	p := &MP4Plugin{}
	p.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	p.RecordConfig.Index.Enable = false
	p.RecordConfig.Index.FlushInterval = 3 * time.Second

	p.applyRecordIndexConfig()

	out := logs.String()
	if !strings.Contains(out, "record.index disabled") {
		t.Errorf("expected a warning about disabled record.index; logs = %q", out)
	}
	if !strings.Contains(out, "cannot be recovered") {
		t.Errorf("warning must explain that crash recovery is disabled; logs = %q", out)
	}
	cfg := mp4pkg.GetIndexConfig()
	if cfg.Enable {
		t.Error("disabled index config must still be injected as disabled")
	}
	if cfg.FlushInterval != 3*time.Second {
		t.Errorf("flush interval = %v, want 3s (explicit value must be preserved)", cfg.FlushInterval)
	}
}

func TestRecordIndexEnabledDoesNotWarn(t *testing.T) {
	prev := mp4pkg.GetIndexConfig()
	t.Cleanup(func() { mp4pkg.SetIndexConfig(prev) })

	var logs bytes.Buffer
	p := &MP4Plugin{}
	p.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	p.RecordConfig.Index.Enable = true
	p.RecordConfig.Index.FlushInterval = time.Second

	p.applyRecordIndexConfig()

	if out := logs.String(); strings.Contains(out, "record.index disabled") {
		t.Errorf("enabled index must not warn about disabled recovery; logs = %q", out)
	}
	if cfg := mp4pkg.GetIndexConfig(); !cfg.Enable {
		t.Error("enabled index config must be injected as enabled")
	}
}
