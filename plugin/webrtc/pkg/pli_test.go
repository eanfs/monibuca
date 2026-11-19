package webrtc

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtcp"
	pion "github.com/pion/webrtc/v4"
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

	unregisterPublisherKeyFrameRequester(pub, conn)
	if got := lookupPublisherKeyFrameRequester(pub); got != nil {
		t.Fatalf("lookup after unregister = %p, want nil", got)
	}

	// 旧连接注销不得删除同一 Publisher 上新注册的连接条目。
	newConn := &MultipleConnection{}
	registerPublisherKeyFrameRequester(pub, conn)
	registerPublisherKeyFrameRequester(pub, newConn)
	unregisterPublisherKeyFrameRequester(pub, conn)
	if got := lookupPublisherKeyFrameRequester(pub); got != newConn {
		t.Fatalf("stale unregister removed newer entry: got %p, want %p", got, newConn)
	}
	unregisterPublisherKeyFrameRequester(pub, newConn)

	// 非法输入不应改变注册表
	registerPublisherKeyFrameRequester(nil, conn)
	registerPublisherKeyFrameRequester(pub, nil)
	unregisterPublisherKeyFrameRequester(nil, conn)
	unregisterPublisherKeyFrameRequester(pub, nil)
	if got := lookupPublisherKeyFrameRequester(pub); got != nil {
		t.Fatalf("nil guard failed: lookup = %p, want nil", got)
	}
	if got := lookupPublisherKeyFrameRequester(nil); got != nil {
		t.Fatalf("nil publisher lookup = %p, want nil", got)
	}
}

// TestEnsurePLIThrottlerIdempotent 验证 ensurePLIThrottler 幂等：
// 重复调用返回同一实例，且创建的是生产默认窗口的节流器。
func TestEnsurePLIThrottlerIdempotent(t *testing.T) {
	conn := &MultipleConnection{}
	first := conn.ensurePLIThrottler()
	if first == nil {
		t.Fatal("ensurePLIThrottler must create a throttler")
	}
	if first.MinInterval != defaultPLIMinInterval || first.Fallback != defaultPLIFallback {
		t.Fatalf("throttler = (%v, %v), want (%v, %v)",
			first.MinInterval, first.Fallback, defaultPLIMinInterval, defaultPLIFallback)
	}
	for i := 0; i < 3; i++ {
		if got := conn.ensurePLIThrottler(); got != first {
			t.Fatalf("ensure call #%d = %p, want same instance %p", i+2, got, first)
		}
	}
	if conn.pliThrottler != first {
		t.Fatalf("pliThrottler = %p, want %p", conn.pliThrottler, first)
	}

	var nilConn *MultipleConnection
	if got := nilConn.ensurePLIThrottler(); got != nil {
		t.Fatalf("nil connection ensure = %p, want nil", got)
	}
}

// TestCanRequestKeyFrameNilThrottler 验证节流器缺失时失败关闭：
// 即使 PeerConnection 已就绪也不放行，且 requestKeyFrame 不 panic、不发送。
// 这钉住 CFClient 等绕过 MultipleConnection.Start 的路径不会退化为
// 无节流的按包速率 PLI。
func TestCanRequestKeyFrameNilThrottler(t *testing.T) {
	now := time.Unix(0, 0)

	// 连接未建立：失败关闭。
	conn := &MultipleConnection{}
	if conn.canRequestKeyFrame(now) {
		t.Fatal("connection without peer connection must not allow keyframe request")
	}
	conn.requestKeyFrame(now) // 不得 panic

	// PeerConnection 就绪但节流器缺失：同样失败关闭。
	pc, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatalf("create peer connection: %v", err)
	}
	defer pc.Close()
	conn = &MultipleConnection{Connection: Connection{PeerConnection: pc}}
	if conn.canRequestKeyFrame(now) {
		t.Fatal("nil throttler must fail closed even with peer connection")
	}
	conn.requestKeyFrame(now) // 不得 panic / 不得发送 PLI

	var nilConn *MultipleConnection
	if nilConn.canRequestKeyFrame(now) {
		t.Fatal("nil connection must not allow keyframe request")
	}
	nilConn.requestKeyFrame(now) // 不得 panic
}

// TestCanRequestKeyFrameUsesThrottlerWindow 验证初始化后的判定入口
// 与生产 PLI 路径共用 500ms 硬窗口。
func TestCanRequestKeyFrameUsesThrottlerWindow(t *testing.T) {
	pc, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatalf("create peer connection: %v", err)
	}
	defer pc.Close()
	conn := &MultipleConnection{Connection: Connection{PeerConnection: pc}}
	if conn.ensurePLIThrottler() == nil {
		t.Fatal("ensurePLIThrottler must create a throttler")
	}
	now := time.Unix(0, 0)
	if !conn.canRequestKeyFrame(now) {
		t.Fatal("first request after ensure must pass")
	}
	if conn.canRequestKeyFrame(now.Add(100 * time.Millisecond)) {
		t.Fatal("request inside 500ms window must be blocked")
	}
	if !conn.canRequestKeyFrame(now.Add(600 * time.Millisecond)) {
		t.Fatal("request after window must pass")
	}
}
