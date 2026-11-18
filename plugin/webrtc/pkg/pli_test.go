package webrtc

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"m7s.live/v5"
)

// TestPLIThrottlerWindow 验证最小间隔窗口：窗口内拒绝，窗口过后放行。
func TestPLIThrottlerWindow(t *testing.T) {
	now := time.Unix(0, 0)
	p := NewPLIThrottler(500*time.Millisecond, 5*time.Second)
	if !p.Allow(now, now) {
		t.Fatal("first request must pass")
	}
	if p.Allow(now.Add(100*time.Millisecond), now) {
		t.Fatal("inside window must be blocked")
	}
	if !p.Allow(now.Add(600*time.Millisecond), now) {
		t.Fatal("after window must pass")
	}
}

// TestPLIThrottlerFallback 验证兜底：最小间隔未到，但已很久没有关键帧时放行。
func TestPLIThrottlerFallback(t *testing.T) {
	now := time.Unix(0, 0)
	p := NewPLIThrottler(time.Hour, 5*time.Second)
	if !p.Allow(now, now) {
		t.Fatal("first request must pass")
	}
	if p.Allow(now.Add(time.Second), now.Add(-10*time.Second)) != true {
		t.Fatal("fallback must fire when no keyframe for > fallback")
	}
}

// TestPLIThrottlerFirstRequest 验证零值 lastSent 时首个请求直接放行。
func TestPLIThrottlerFirstRequest(t *testing.T) {
	now := time.Unix(0, 0)
	p := NewPLIThrottler(500*time.Millisecond, 5*time.Second)
	if !p.Allow(now, time.Unix(0, 0)) {
		t.Fatal("zero lastSent must pass on first request")
	}
	if p.Allow(now, time.Unix(0, 0)) {
		t.Fatal("second request in the same instant must be blocked")
	}
}

// TestPLIThrottlerConcurrentViewers 验证节流预算按 Publisher 共享：
// N 个观众并发请求，窗口内只允许一次（Ruling 7：不许按观众放大 PLI 速率）。
//
// 场景取生产最坏情况：Fallback=5s 且 lastKeyFrame 已陈旧（>5s 没见到 IDR），
// 若调用方误用 Allow(now, lastKeyFrame)，Fallback 分支会让每次请求都绕过
// 最小间隔；生产路径必须用严格调用 Allow(now, now)，只保留 500ms 硬窗口，
// 即使长期无关键帧也只放行一次。
func TestPLIThrottlerConcurrentViewers(t *testing.T) {
	now := time.Unix(0, 0)
	staleLastKeyFrame := now.Add(-10 * time.Second) // 兜底条件已成立（> Fallback）

	// 对照：brief 的 Allow(now, lastKeyFrame) 语义在同样状态下会放行兜底。
	control := NewPLIThrottler(time.Hour, 5*time.Second)
	if !control.Allow(now, now) {
		t.Fatal("control: first request must pass")
	}
	if !control.Allow(now.Add(time.Second), staleLastKeyFrame) {
		t.Fatal("control: fallback must fire with stale keyframe (brief semantics)")
	}

	// 生产路径：Allow(now, now) 使 Fallback 分支不可达。
	p := NewPLIThrottler(time.Hour, 5*time.Second)
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.Allow(now, now) {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != 1 {
		t.Fatalf("want exactly 1 allowed request across viewers, got %d", got)
	}
}

// TestIsSevereLoss 验证严重丢包阈值：FractionLost 以 1/256 为单位，
// 10% = 25/256，严格大于 25 才算严重丢包。
func TestIsSevereLoss(t *testing.T) {
	cases := []struct {
		name   string
		report *rtcp.ReceiverReport
		want   bool
	}{
		{name: "nil", report: nil, want: false},
		{name: "no reports", report: &rtcp.ReceiverReport{}, want: false},
		{name: "no loss", report: &rtcp.ReceiverReport{Reports: []rtcp.ReceptionReport{{FractionLost: 0}}}, want: false},
		{name: "exactly 10 percent (25/256)", report: &rtcp.ReceiverReport{Reports: []rtcp.ReceptionReport{{FractionLost: 25}}}, want: false},
		{name: "above 10 percent (26/256)", report: &rtcp.ReceiverReport{Reports: []rtcp.ReceptionReport{{FractionLost: 26}}}, want: true},
		{name: "one severe block among many", report: &rtcp.ReceiverReport{Reports: []rtcp.ReceptionReport{{FractionLost: 1}, {FractionLost: 200}}}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSevereLoss(tc.report); got != tc.want {
				t.Fatalf("isSevereLoss(%v) = %v, want %v", tc.report, got, tc.want)
			}
		})
	}
}

// TestPublisherKeyFrameRequesterRegistry 验证按 Publisher 注册/查找/注销关键帧请求目标。
// 使用零值 Publisher 与零值 MultipleConnection，不触发任何网络写入。
func TestPublisherKeyFrameRequesterRegistry(t *testing.T) {
	pub := &m7s.Publisher{}
	conn := &MultipleConnection{}

	registerPublisherKeyFrameRequester(pub, conn)
	if got := lookupPublisherKeyFrameRequester(pub); got != conn {
		t.Fatalf("lookup after register = %p, want %p", got, conn)
	}

	unregisterPublisherKeyFrameRequester(pub)
	if got := lookupPublisherKeyFrameRequester(pub); got != nil {
		t.Fatalf("lookup after unregister = %p, want nil", got)
	}

	// 非法输入不应改变注册表
	registerPublisherKeyFrameRequester(nil, conn)
	registerPublisherKeyFrameRequester(pub, nil)
	if got := lookupPublisherKeyFrameRequester(pub); got != nil {
		t.Fatalf("nil guard failed: lookup = %p, want nil", got)
	}
	if got := lookupPublisherKeyFrameRequester(nil); got != nil {
		t.Fatalf("nil publisher lookup = %p, want nil", got)
	}
}
