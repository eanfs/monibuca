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
			name:   "录制仍活跃 -> 等待（不并行续录）",
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
	it := &resumeIntent{streamPath: "live/a", segments: 1}
	s.put(it)
	if got := len(s.snapshot()); got != 1 {
		t.Fatalf("snapshot len = %d, want 1", got)
	}
	if !s.isCurrent(it) {
		t.Fatal("isCurrent(just put) = false, want true")
	}
	if !s.remove("live/a") {
		t.Fatal("remove(existing) = false, want true")
	}
	if s.isCurrent(it) {
		t.Fatal("isCurrent(removed) = true, want false")
	}
	if s.remove("live/a") {
		t.Fatal("remove(absent) = true, want false")
	}
	if got := len(s.snapshot()); got != 0 {
		t.Fatalf("snapshot len after remove = %d, want 0", got)
	}
}

// 看护 Tick 的 snapshot 过期后（用户已 stop 或已重新 start），isCurrent 必须否定
// 旧意图，否则正在收尾的旧快照会把用户刚停掉的录制重新拉起。
func TestAutoResumeStoreIsCurrent(t *testing.T) {
	var s autoResumeStore
	oldIt := &resumeIntent{streamPath: "live/a", deadline: time.Now().Add(time.Hour), segments: 1}
	s.put(oldIt)
	if !s.isCurrent(oldIt) {
		t.Fatal("isCurrent(just put) = false, want true")
	}
	// /mp4/api/start 用新意图替换旧意图。
	newIt := &resumeIntent{streamPath: "live/a", deadline: time.Now().Add(2 * time.Hour), segments: 1}
	s.put(newIt)
	if s.isCurrent(oldIt) {
		t.Fatal("isCurrent(replaced) = true, want false")
	}
	if !s.isCurrent(newIt) {
		t.Fatal("isCurrent(new) = false, want true")
	}
	// /mp4/api/stop 删除意图。
	s.remove("live/a")
	if s.isCurrent(newIt) {
		t.Fatal("isCurrent(removed) = true, want false")
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

	t.Run("录制活跃 -> 不续录、清零等待计时且不探测流状态", func(t *testing.T) {
		p := newPlugin()
		p.resumeStore.put(&resumeIntent{streamPath: "live/a", deadline: now.Add(time.Hour), segments: 1,
			waitingSince: now.Add(-time.Minute)})
		var resumed int
		p.autoResumeTickWith(
			func(string) bool { return true }, // 活跃
			func(string) bool { // 录制活跃时决策不依赖流状态，探测纯属浪费
				t.Fatal("must not probe stream state while recording is active")
				return true
			},
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
	conf := config.Record{FilePath: ".", Duration: time.Minute}

	p.registerResumeIntent("live/off", conf)
	if n := len(p.resumeStore.snapshot()); n != 0 {
		t.Fatalf("intent registered while disabled: %d", n)
	}

	p.RecordConfig.AutoResume.Enable = true
	p.registerResumeIntent("live/infinite", config.Record{FilePath: "."})
	if n := len(p.resumeStore.snapshot()); n != 0 {
		t.Fatalf("intent registered for duration=0: %d", n)
	}

	before := time.Now()
	p.registerResumeIntent("live/a", conf)
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

// TestAutoResumeConfigDefaults 钉住默认值：自动续录默认开启、等待窗口默认 10m。
//
// 默认开启是刻意的：录制任务挂在 publisher 上，断流即终止，业务补偿不可靠
// （2026-11-21 现场补偿滞后约 9 分钟）。MaxWait 必须是 10m 而不是 5m：现场
// 断流后部分流 5m40s 才恢复，5m 会在恢复前放弃续录。
func TestAutoResumeConfigDefaults(t *testing.T) {
	p := &MP4Plugin{}
	config.Parse(p, nil)
	got := p.RecordConfig.AutoResume
	if !got.Enable {
		t.Fatal("autoresume.enable default = false, want true")
	}
	if got.MaxWait != 10*time.Minute {
		t.Fatalf("autoresume.maxwait default = %v, want 10m", got.MaxWait)
	}
	if got.Interval != 5*time.Second {
		t.Fatalf("autoresume.interval default = %v, want 5s", got.Interval)
	}
	if got.MaxSegments != 0 {
		t.Fatalf("autoresume.maxsegments default = %d, want 0", got.MaxSegments)
	}
}

// 看护 Tick 决策期间用户 stop（删除意图）时，本轮不得再拉起录制。
// 用 streamUp 回调模拟这段并发窗口。
func TestAutoResumeTickSkipsIntentRemovedDuringTick(t *testing.T) {
	p := newAutoResumeTestPlugin()
	p.RecordConfig.AutoResume.Enable = true
	now := time.Now()
	p.resumeStore.put(&resumeIntent{streamPath: "live/a", deadline: now.Add(time.Hour), segments: 1})
	p.autoResumeTickWith(
		func(string) bool { return false }, // 录制已结束
		func(string) bool { // 模拟 /mp4/api/stop 在 tick 决策期间删除意图
			p.forgetResumeIntent("live/a")
			return true
		},
		func(*resumeIntent) { t.Fatal("intent removed during tick must not be resumed") },
		now,
	)
}

// TestAutoResumeConfigBinding 钉住 yaml 键 mp4.record.autoresume.{enable,maxwait}
// 的绑定：默认开启后，现场必须能显式 enable: false 关掉。
func TestAutoResumeConfigBinding(t *testing.T) {
	p := &MP4Plugin{}
	config.Parse(p, map[string]any{
		"record": map[string]any{
			"autoresume": map[string]any{
				"enable":  false,
				"maxwait": "2m",
			},
		},
	})
	if p.RecordConfig.AutoResume.Enable {
		t.Fatal("autoresume.enable: false not bound")
	}
	if p.RecordConfig.AutoResume.MaxWait != 2*time.Minute {
		t.Fatalf("autoresume.maxwait = %v, want 2m", p.RecordConfig.AutoResume.MaxWait)
	}
}

// TestAutoResumeSlowRecoveryWithinMaxWait 复现现场时序：录制结束后流 5m40s 才恢复。
// 默认 MaxWait=10m 时期间必须保持等待，恢复后立即续录；若默认退回 5m 本用例会失败。
func TestAutoResumeSlowRecoveryWithinMaxWait(t *testing.T) {
	p := &MP4Plugin{}
	config.Parse(p, nil)
	start := time.Now()
	p.registerResumeIntent("live/slow", config.Record{FilePath: ".", Duration: time.Hour})
	snap := p.resumeStore.snapshot()
	if len(snap) != 1 {
		t.Fatalf("intent not registered: %+v", snap)
	}
	it := snap[0]
	// 录制在 start+1m 结束，之后流一直不在线。
	it.waitingSince = start.Add(time.Minute)

	var resumed int
	p.autoResumeTickWith(
		func(string) bool { return false }, // 录制已结束
		func(string) bool { return false }, // 流仍未恢复
		func(*resumeIntent) { resumed++ },
		start.Add(time.Minute+5*time.Minute+40*time.Second), // 断流 5m40s
	)
	if resumed != 0 {
		t.Fatal("resumed while stream was down")
	}
	if n := len(p.resumeStore.snapshot()); n != 1 {
		t.Fatalf("intent dropped after 5m40s wait (want keep for default 10m): %d left", n)
	}

	// 流恢复后下一轮必须续录。
	p.autoResumeTickWith(
		func(string) bool { return false },
		func(string) bool { return true },
		func(*resumeIntent) { resumed++ },
		start.Add(7*time.Minute),
	)
	if resumed != 1 {
		t.Fatalf("resumed = %d after stream recovered, want 1", resumed)
	}
}
