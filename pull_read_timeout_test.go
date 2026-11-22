package m7s

import (
	"testing"
	"time"

	"m7s.live/v5/pkg/config"
)

// TestGlobalPullReadTimeoutConfigBinding 钉住 "global.pullreadtimeout -> ServerConfig.PullReadTimeout"
// 的配置绑定。
//
// m7s 的配置解析对未知键静默跳过（pkg/config ParseUserFile 的 config.Has 判断），
// 键名写错或字段缺失时表现为"配了不生效、启动也不报错"——与 waitclosetimeout
// 属于同一类陷阱。这里直接走配置解析入口，删掉字段或改错键名都会让本用例变红。
func TestGlobalPullReadTimeoutConfigBinding(t *testing.T) {
	s := &Server{}
	config.Parse(&s.ServerConfig, map[string]any{"pullreadtimeout": "30s"})
	if s.PullReadTimeout != 30*time.Second {
		t.Fatalf("global.pullreadtimeout not bound: got %v, want 30s", s.PullReadTimeout)
	}
}

// TestPullReadTimeoutConfigKeyNormalization 确认真实配置路径（ParseUserFile）的键名
// 归一化规则下仍能命中：解析前会删掉 - 和 _，所以 pull-read-timeout /
// pull_read_timeout 也应绑定到 PullReadTimeout。
//
// 注意必须走 Config.ParseUserFile：包级 config.Parse 走的是 ParseModifyFile，
// 那条路径不做键名归一化，用它测归一化会误判。
func TestPullReadTimeoutConfigKeyNormalization(t *testing.T) {
	for _, key := range []string{"pullreadtimeout", "pull-read-timeout", "pull_read_timeout"} {
		t.Run(key, func(t *testing.T) {
			s := &Server{}
			var c config.Config
			c.Parse(&s.ServerConfig, "GLOBAL")
			c.ParseUserFile(map[string]any{key: "45s"})
			if s.PullReadTimeout != 45*time.Second {
				t.Fatalf("key %q not bound: got %v, want 45s", key, s.PullReadTimeout)
			}
		})
	}
}

// TestEffectivePullReadTimeoutUsesGlobalConfig 代理未显式配置时取全局默认。
func TestEffectivePullReadTimeoutUsesGlobalConfig(t *testing.T) {
	s := &Server{}
	s.PullReadTimeout = 30 * time.Second
	if got := s.effectivePullReadTimeout(0); got != 30*time.Second {
		t.Fatalf("effectivePullReadTimeout(0) = %v, want global 30s", got)
	}
}

// TestEffectivePullReadTimeoutGlobalIsFloor 全局值是下限：比它小的代理取值
// （含旧版 API 写进 DB 的内置默认 10s）必须被提升。
func TestEffectivePullReadTimeoutGlobalIsFloor(t *testing.T) {
	s := &Server{}
	s.PullReadTimeout = 30 * time.Second
	for _, explicit := range []time.Duration{0, 5 * time.Second, 10 * time.Second} {
		if got := s.effectivePullReadTimeout(explicit); got != 30*time.Second {
			t.Fatalf("effectivePullReadTimeout(%v) = %v, want floor 30s", explicit, got)
		}
	}
}

// TestEffectivePullReadTimeoutLargerExplicitWins 代理配置大于全局下限时保留自身值。
func TestEffectivePullReadTimeoutLargerExplicitWins(t *testing.T) {
	s := &Server{}
	s.PullReadTimeout = 30 * time.Second
	if got := s.effectivePullReadTimeout(60 * time.Second); got != 60*time.Second {
		t.Fatalf("effectivePullReadTimeout(60s) = %v, want explicit 60s", got)
	}
}

// TestEffectivePullReadTimeoutNoGlobalKeepsExplicit 未配置全局下限时不改变既有行为。
func TestEffectivePullReadTimeoutNoGlobalKeepsExplicit(t *testing.T) {
	s := &Server{}
	if got := s.effectivePullReadTimeout(5 * time.Second); got != 5*time.Second {
		t.Fatalf("effectivePullReadTimeout(5s) = %v, want explicit 5s", got)
	}
}

// TestEffectivePullReadTimeoutFallsBackToBuiltin 全局未配置时保持既有行为：回落内置 10s。
// 这条钉住"新增配置项不改变未配置部署的运行时行为"。
func TestEffectivePullReadTimeoutFallsBackToBuiltin(t *testing.T) {
	s := &Server{}
	if got := s.effectivePullReadTimeout(0); got != defaultPullProxyReadTimeout {
		t.Fatalf("effectivePullReadTimeout(0) = %v, want builtin %v", got, defaultPullProxyReadTimeout)
	}
	if defaultPullProxyReadTimeout != 10*time.Second {
		t.Fatalf("builtin pull read timeout = %v, want 10s (与 plugin/rtsp DefaultReadTimeout 保持一致)", defaultPullProxyReadTimeout)
	}
}

// TestEffectivePullReadTimeoutNilServer nil Server（零值/中间态）不得 panic，仍回落内置默认。
func TestEffectivePullReadTimeoutNilServer(t *testing.T) {
	var s *Server
	if got := s.effectivePullReadTimeout(0); got != defaultPullProxyReadTimeout {
		t.Fatalf("nil server effectivePullReadTimeout(0) = %v, want %v", got, defaultPullProxyReadTimeout)
	}
}

// TestInitializeWithServerAppliesGlobalReadTimeout DB 历史数据 read_timeout=0 的代理
// 必须在加载时被填上全局默认：否则 RTSP 连接回落到内置 10s，10 秒级网络抖动即
// 断开 -> unpublish -> 录制终止（本次现场问题的直接触发点）。
func TestInitializeWithServerAppliesGlobalReadTimeout(t *testing.T) {
	s := &Server{}
	s.PullReadTimeout = 30 * time.Second
	proxy := &PullProxyConfig{Type: "rtsp", ReadTimeout: 0}
	proxy.InitializeWithServer(s)
	if proxy.ReadTimeout != 30*time.Second {
		t.Fatalf("proxy.ReadTimeout = %v, want global 30s", proxy.ReadTimeout)
	}
}

// TestInitializeWithServerAppliesGlobalFloorToLegacyDBTimeout 现场关键路径：
// 旧版 API 在请求未指定 readtimeout 时会把内置 10s 写进 DB，加载时必须被全局下限
// 提升，否则 global.pullreadtimeout 对存量代理完全不生效（2026-11-21 现场即如此）。
func TestInitializeWithServerAppliesGlobalFloorToLegacyDBTimeout(t *testing.T) {
	s := &Server{}
	s.PullReadTimeout = 30 * time.Second
	proxy := &PullProxyConfig{Type: "rtsp", ReadTimeout: 10 * time.Second}
	proxy.InitializeWithServer(s)
	if proxy.ReadTimeout != 30*time.Second {
		t.Fatalf("legacy DB read_timeout 10s = %v, want global floor 30s", proxy.ReadTimeout)
	}
}

// TestInitializeWithServerKeepsLargerExplicitReadTimeout 大于全局下限的代理值不被覆盖。
func TestInitializeWithServerKeepsLargerExplicitReadTimeout(t *testing.T) {
	s := &Server{}
	s.PullReadTimeout = 30 * time.Second
	proxy := &PullProxyConfig{Type: "rtsp", ReadTimeout: 60 * time.Second}
	proxy.InitializeWithServer(s)
	if proxy.ReadTimeout != 60*time.Second {
		t.Fatalf("proxy.ReadTimeout = %v, want explicit 60s", proxy.ReadTimeout)
	}
}

// TestInitializeWithServerDefaultsToBuiltin 全局未配置时把 0 填成内置 10s（与原行为一致）。
func TestInitializeWithServerDefaultsToBuiltin(t *testing.T) {
	s := &Server{}
	proxy := &PullProxyConfig{Type: "rtsp"}
	proxy.InitializeWithServer(s)
	if proxy.ReadTimeout != 10*time.Second {
		t.Fatalf("proxy.ReadTimeout = %v, want 10s", proxy.ReadTimeout)
	}
}
