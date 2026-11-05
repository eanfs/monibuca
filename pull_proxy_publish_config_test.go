package m7s

import (
	"testing"
	"time"

	"m7s.live/v5/pkg/config"
)

// stop_on_idle=false 的拉流代理是常驻拉流：既不能延迟关闭，也不能被
// publish.idletimeout 关闭。后者是 v5.3.9 录像失败问题的回归点。
func TestBuildPullPublishConfigKeepAliveDisablesIdleTimeouts(t *testing.T) {
	base := config.Publish{
		DelayCloseTimeout: 15 * time.Minute,
		IdleTimeout:       10 * time.Minute,
		PubAudio:          false,
		PubVideo:          true,
	}
	got := buildPullPublishConfig(base, false, true)

	if got.DelayCloseTimeout != 0 {
		t.Fatalf("keep-alive proxy DelayCloseTimeout = %v, want 0", got.DelayCloseTimeout)
	}
	if got.IdleTimeout != 0 {
		t.Fatalf("keep-alive proxy IdleTimeout = %v, want 0 (publisher 会被 publish.idletimeout 杀掉)", got.IdleTimeout)
	}
	if !got.PubAudio {
		t.Fatal("buildPullPublishConfig must apply audio flag")
	}
	if !got.PubVideo {
		t.Fatal("buildPullPublishConfig must preserve other publish fields")
	}
	if base.DelayCloseTimeout == 0 || base.IdleTimeout == 0 {
		t.Fatal("buildPullPublishConfig must not mutate its input")
	}
}

// stop_on_idle=true 的按需拉流代理保持既有语义：订阅者离开 5s 后关闭，
// 且保留 idle 超时（从未有订阅者时同样按空闲处理）。
func TestBuildPullPublishConfigStopOnIdleKeepsIdleTimeout(t *testing.T) {
	base := config.Publish{
		DelayCloseTimeout: 15 * time.Minute,
		IdleTimeout:       10 * time.Minute,
		PubAudio:          true,
	}
	got := buildPullPublishConfig(base, true, false)

	if got.DelayCloseTimeout != 5*time.Second {
		t.Fatalf("stop-on-idle proxy DelayCloseTimeout = %v, want 5s", got.DelayCloseTimeout)
	}
	if got.IdleTimeout != base.IdleTimeout {
		t.Fatalf("stop-on-idle proxy IdleTimeout = %v, want %v", got.IdleTimeout, base.IdleTimeout)
	}
	if got.PubAudio {
		t.Fatal("buildPullPublishConfig must apply audio flag")
	}
}

// Pull() 必须经 publishConfig() 取配置，避免"只测 helper、调用点被绕过"：
// 这里直接模拟代理的发布配置构造点。
func TestPullProxyPublishConfigFollowsStopOnIdle(t *testing.T) {
	p := &Plugin{}
	p.config.Publish = config.Publish{
		DelayCloseTimeout: 15 * time.Minute,
		IdleTimeout:       10 * time.Minute,
	}
	proxy := &BasePullProxy{
		Plugin:          p,
		PullProxyConfig: &PullProxyConfig{StopOnIdle: false, Audio: true},
	}

	got := proxy.publishConfig()
	if got.DelayCloseTimeout != 0 || got.IdleTimeout != 0 {
		t.Fatalf("keep-alive publishConfig = {delay:%v idle:%v}, want {0 0}",
			got.DelayCloseTimeout, got.IdleTimeout)
	}

	proxy.StopOnIdle = true
	got = proxy.publishConfig()
	if got.DelayCloseTimeout != 5*time.Second {
		t.Fatalf("stop-on-idle DelayCloseTimeout = %v, want 5s", got.DelayCloseTimeout)
	}
	if got.IdleTimeout != 10*time.Minute {
		t.Fatalf("stop-on-idle IdleTimeout = %v, want 10m", got.IdleTimeout)
	}
}
