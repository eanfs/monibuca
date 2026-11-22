package plugin_mp4

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"m7s.live/v5/pkg/config"
)

func newAutoResumeTestPlugin() *MP4Plugin {
	p := &MP4Plugin{}
	p.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return p
}

// decideResume 的决策矩阵：覆盖到期/活跃/段数上限/流未恢复(未超时/超时)/可续录。
func TestDecideResume(t *testing.T) {
	now := time.Now()
	base := func() *resumeIntent {
		return &resumeIntent{streamPath: "live/a", deadline: now.Add(time.Hour), segments: 1}
	}
	cases := []struct {
		name        string
		intent      func() *resumeIntent
		active      bool
		streamUp    bool
		maxWait     time.Duration
		maxSegments int
		want        resumeAction
	}{
		{
			name:     "deadline 已到 -> 放弃（即使流在线）",
			intent:   func() *resumeIntent { it := base(); it.deadline = now.Add(-time.Second); return it },
			streamUp: true, want: resumeActionGiveUp,
		},
		{
			name:   "录制仍活跃 -> 等待（含 trailer/upload 收尾）",
			intent: base, active: true, streamUp: true, want: resumeActionWait,
		},
		{
			name:     "段数达到上限 -> 放弃",
			intent:   func() *resumeIntent { it := base(); it.segments = 2; return it },
			streamUp: true, maxSegments: 2, want: resumeActionGiveUp,
		},
		{
			name:    "流未恢复且未超时 -> 等待",
			intent:  func() *resumeIntent { it := base(); it.waitingSince = now.Add(-time.Minute); return it },
			maxWait: 5 * time.Minute, want: resumeActionWait,
		},
		{
			name:    "流未恢复且超过 MaxWait -> 放弃",
			intent:  func() *resumeIntent { it := base(); it.waitingSince = now.Add(-6 * time.Minute); return it },
			maxWait: 5 * time.Minute, want: resumeActionGiveUp,
		},
		{
			name:    "MaxWait=0 时流未恢复也不放弃（由 deadline 兜底）",
			intent:  func() *resumeIntent { it := base(); it.waitingSince = now.Add(-24 * time.Hour); return it },
			maxWait: 0, want: resumeActionWait,
		},
		{
			name:     "流已恢复且录制结束 -> 续录",
			intent:   func() *resumeIntent { it := base(); it.waitingSince = now.Add(-time.Minute); return it },
			streamUp: true, maxWait: 5 * time.Minute, want: resumeActionResume,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := decideResume(c.intent(), c.active, c.streamUp, now, c.maxWait, c.maxSegments)
			if got != c.want {
				t.Fatalf("decideResume() = %v (%s), want %v", got, reason, c.want)
			}
		})
	}
}

// 意图表并发安全 + 基本增删语义。
func TestAutoResumeStore(t *testing.T) {
	var s autoResumeStore
	s.put(&resumeIntent{streamPath: "live/a", segments: 1})
	if got := len(s.snapshot()); got != 1 {
		t.Fatalf("snapshot len = %d, want 1", got)
	}
	if !s.remove("live/a") {
		t.Fatal("remove(existing) = false, want true")
	}
	if s.remove("live/a") {
		t.Fatal("remove(absent) = true, want false")
	}
	if got := len(s.snapshot()); got != 0 {
		t.Fatalf("snapshot len after remove = %d, want 0", got)
	}
}

// autoResumeTickWith 的一轮处置：活跃等待、流恢复续录、超时清理、到期清理。
func TestAutoResumeTickWith(t *testing.T) {
	now := time.Now()

	newPlugin := func() *MP4Plugin {
		p := newAutoResumeTestPlugin()
		p.RecordConfig.AutoResume.Enable = true
		p.RecordConfig.AutoResume.MaxWait = 5 * time.Minute
		return p
	}

	t.Run("录制活跃 -> 不续录且清零等待计时", func(t *testing.T) {
		p := newPlugin()
		p.resumeStore.put(&resumeIntent{streamPath: "live/a", deadline: now.Add(time.Hour), segments: 1,
			waitingSince: now.Add(-time.Minute)})
		var resumed int
		p.autoResumeTickWith(
			func(string) bool { return true }, // 活跃
			func(string) bool { return true }, // 流在线
			func(*resumeIntent) { resumed++ },
			now,
		)
		if resumed != 0 {
			t.Fatalf("resumed = %d, want 0 (录制活跃时不得并发续录)", resumed)
		}
		for _, it := range p.resumeStore.snapshot() {
			if !it.waitingSince.IsZero() {
				t.Fatalf("waitingSince = %v, want zero while active", it.waitingSince)
			}
		}
	})

	t.Run("录制结束且流恢复 -> 续录", func(t *testing.T) {
		p := newPlugin()
		p.resumeStore.put(&resumeIntent{streamPath: "live/a", deadline: now.Add(time.Hour), segments: 1,
			waitingSince: now.Add(-time.Minute)})
		var got []string
		p.autoResumeTickWith(
			func(string) bool { return false },
			func(string) bool { return true },
			func(it *resumeIntent) { got = append(got, it.streamPath) },
			now,
		)
		if len(got) != 1 || got[0] != "live/a" {
			t.Fatalf("resumed = %v, want [live/a]", got)
		}
	})

	t.Run("流未恢复超过 MaxWait -> 清理意图", func(t *testing.T) {
		p := newPlugin()
		p.resumeStore.put(&resumeIntent{streamPath: "live/a", deadline: now.Add(time.Hour), segments: 1,
			waitingSince: now.Add(-6 * time.Minute)})
		p.autoResumeTickWith(
			func(string) bool { return false },
			func(string) bool { return false },
			func(*resumeIntent) { t.Fatal("must not resume when stream is down") },
			now,
		)
		if n := len(p.resumeStore.snapshot()); n != 0 {
			t.Fatalf("intent not cleaned up: %d left", n)
		}
	})

	t.Run("duration 到期 -> 清理意图", func(t *testing.T) {
		p := newPlugin()
		p.resumeStore.put(&resumeIntent{streamPath: "live/a", deadline: now.Add(-time.Second), segments: 1})
		p.autoResumeTickWith(
			func(string) bool { return false },
			func(string) bool { return true },
			func(*resumeIntent) { t.Fatal("must not resume after deadline") },
			now,
		)
		if n := len(p.resumeStore.snapshot()); n != 0 {
			t.Fatalf("expired intent not cleaned up: %d left", n)
		}
	})
}

// 登记规则：未开启不登记；开启但 duration=0（无限录制）不登记；正常登记 segments=1 且 deadline 正确。
func TestRegisterResumeIntent(t *testing.T) {
	p := newAutoResumeTestPlugin()
	conf := config.Record{FilePath: "."}

	p.registerResumeIntent("live/off", conf, time.Minute)
	if n := len(p.resumeStore.snapshot()); n != 0 {
		t.Fatalf("intent registered while disabled: %d", n)
	}

	p.RecordConfig.AutoResume.Enable = true
	p.registerResumeIntent("live/infinite", conf, 0)
	if n := len(p.resumeStore.snapshot()); n != 0 {
		t.Fatalf("intent registered for duration=0: %d", n)
	}

	before := time.Now()
	p.registerResumeIntent("live/a", conf, time.Minute)
	snap := p.resumeStore.snapshot()
	if len(snap) != 1 || snap[0].streamPath != "live/a" {
		t.Fatalf("intent not registered: %+v", snap)
	}
	if snap[0].segments != 1 {
		t.Fatalf("segments = %d, want 1 (首段计入)", snap[0].segments)
	}
	if d := snap[0].deadline.Sub(before); d < time.Minute-2*time.Second || d > time.Minute+2*time.Second {
		t.Fatalf("deadline offset = %v, want ~1m", d)
	}

	p.forgetResumeIntent("live/a")
	if n := len(p.resumeStore.snapshot()); n != 0 {
		t.Fatalf("forgetResumeIntent left %d intents", n)
	}
}

// 配置兜底：interval<=0 回落 5s；maxwait 负值按 0（不限）处理。
func TestAutoResumeConfigFallback(t *testing.T) {
	if got := effectiveInterval(0); got != 5*time.Second {
		t.Fatalf("effectiveInterval(0) = %v, want 5s", got)
	}
	if got := effectiveInterval(-time.Second); got != 5*time.Second {
		t.Fatalf("effectiveInterval(-1s) = %v, want 5s", got)
	}
	if got := effectiveInterval(time.Second); got != time.Second {
		t.Fatalf("effectiveInterval(1s) = %v, want 1s", got)
	}
	if got := effectiveMaxWait(-time.Second); got != 0 {
		t.Fatalf("effectiveMaxWait(-1s) = %v, want 0", got)
	}
}
