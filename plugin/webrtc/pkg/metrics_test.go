package webrtc

import (
	"strings"
	"sync"
	"testing"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/rtcp"
)

// fakeBandwidthEstimator 是最小化的 cc.BandwidthEstimator 实现，
// 用于在不启动真实 PeerConnection 的情况下验证按连接绑定估计器的逻辑。
type fakeBandwidthEstimator struct {
	mu            sync.Mutex
	target        int
	onChange      func(int)
	registrations int
}

var _ cc.BandwidthEstimator = (*fakeBandwidthEstimator)(nil)

func (f *fakeBandwidthEstimator) AddStream(_ *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	return writer
}

func (f *fakeBandwidthEstimator) WriteRTCP(_ []rtcp.Packet, _ interceptor.Attributes) error {
	return nil
}

func (f *fakeBandwidthEstimator) GetTargetBitrate() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.target
}

func (f *fakeBandwidthEstimator) OnTargetBitrateChange(cb func(bitrate int)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onChange = cb
	f.registrations++
}

func (f *fakeBandwidthEstimator) GetStats() map[string]any {
	return nil
}

func (f *fakeBandwidthEstimator) Close() error {
	return nil
}

// fire 模拟 gcc 在目标码率变化时触发的回调。
func (f *fakeBandwidthEstimator) fire(bitrate int) {
	f.mu.Lock()
	cb := f.onChange
	f.mu.Unlock()
	if cb != nil {
		cb(bitrate)
	}
}

func (f *fakeBandwidthEstimator) registrationCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registrations
}

func TestStreamMetrics(t *testing.T) {
	m := &StreamMetrics{}
	m.AddSent(100)
	m.AddSent(250)
	m.TargetBitrate.Store(2_000_000)
	m.SendBitrate.Store(1_500_000)
	m.PLIRequests.Store(3)
	m.RTT.Store(42)
	m.Jitter.Store(7)

	snap := m.Snapshot()
	if snap.PacketsSent != 2 {
		t.Errorf("PacketsSent = %d, want 2", snap.PacketsSent)
	}
	if snap.BytesSent != 350 {
		t.Errorf("BytesSent = %d, want 350", snap.BytesSent)
	}
	if snap.TargetBitrate != 2_000_000 {
		t.Errorf("TargetBitrate = %d, want 2000000", snap.TargetBitrate)
	}
	if snap.SendBitrate != 1_500_000 {
		t.Errorf("SendBitrate = %d, want 1500000", snap.SendBitrate)
	}
	if snap.PLIRequests != 3 {
		t.Errorf("PLIRequests = %d, want 3", snap.PLIRequests)
	}
	if snap.RTT != 42 {
		t.Errorf("RTT = %d, want 42", snap.RTT)
	}
	if snap.Jitter != 7 {
		t.Errorf("Jitter = %d, want 7", snap.Jitter)
	}

	line := snap.LogLine()
	for _, key := range []string{
		"target_bitrate=2000000",
		"send_bitrate=1500000",
		"packets_sent=2",
		"packets_lost=0",
		"pli_requests=3",
		"rtt=42",
		"jitter=7",
	} {
		if !strings.Contains(line, key) {
			t.Errorf("LogLine() = %q, missing %q", line, key)
		}
	}
}

// TestHandleViewerRTCP 钉住两处 RTCP 读循环共用的处理函数：
// 无 Publisher 目标时不 panic、不虚增 PLI；ReceiverReport 仍写入 metrics。
func TestHandleViewerRTCP(t *testing.T) {
	m := &StreamMetrics{}
	handleViewerRTCP(nil, []rtcp.Packet{
		&rtcp.PictureLossIndication{},
		&rtcp.ReceiverReport{Reports: []rtcp.ReceptionReport{{SSRC: 1, TotalLost: 9}}},
	}, m)
	if got := m.Snapshot().PacketsLost; got != 9 {
		t.Fatalf("PacketsLost = %d, want 9", got)
	}
	if got := m.Snapshot().PLIRequests; got != 0 {
		t.Fatalf("PLIRequests = %d, want 0 without a publisher target", got)
	}
	handleViewerRTCP(nil, []rtcp.Packet{&rtcp.PictureLossIndication{}}, nil)
}

// TestStreamMetricsNilSafe 保证未初始化/空指标不会 panic（埋点路径可能早于 Start）。
func TestStreamMetricsNilSafe(t *testing.T) {
	var m *StreamMetrics
	m.AddSent(10)
	m.ObserveReceiverReport(&rtcp.ReceiverReport{})
	if snap := m.Snapshot(); snap != (MetricsSnapshot{}) {
		t.Errorf("nil Snapshot = %+v, want zero", snap)
	}
}

// TestStreamMetricsLossLatest 钉住 Ruling 6：ReceiverReport.TotalLost 已经是累计值，
// 同一 SSRC 只保留最新值，多个 SSRC 的最新值相加，绝不能把每份报告累加。
func TestStreamMetricsLossLatest(t *testing.T) {
	m := &StreamMetrics{}
	m.ObserveReceiverReport(&rtcp.ReceiverReport{
		Reports: []rtcp.ReceptionReport{{SSRC: 1, TotalLost: 5}},
	})
	if got := m.Snapshot().PacketsLost; got != 5 {
		t.Fatalf("PacketsLost after first report = %d, want 5", got)
	}

	// 同一个 SSRC 的后续报告是累计值，不能 5+9=14。
	m.ObserveReceiverReport(&rtcp.ReceiverReport{
		Reports: []rtcp.ReceptionReport{{SSRC: 1, TotalLost: 9}},
	})
	if got := m.Snapshot().PacketsLost; got != 9 {
		t.Fatalf("PacketsLost after latest same-SSRC report = %d, want 9", got)
	}

	// 新增 SSRC 2，最新值 3，总和 9+3=12。
	m.ObserveReceiverReport(&rtcp.ReceiverReport{
		Reports: []rtcp.ReceptionReport{{SSRC: 2, TotalLost: 3}},
	})
	if got := m.Snapshot().PacketsLost; got != 12 {
		t.Fatalf("PacketsLost after adding SSRC 2 = %d, want 12", got)
	}

	// SSRC 1 的最新值变为 10，总和 10+3=13。
	m.ObserveReceiverReport(&rtcp.ReceiverReport{
		Reports: []rtcp.ReceptionReport{{SSRC: 1, TotalLost: 10}},
	})
	if got := m.Snapshot().PacketsLost; got != 13 {
		t.Fatalf("PacketsLost after SSRC 1 update = %d, want 13", got)
	}

	// 空报告不应清零已有统计。
	m.ObserveReceiverReport(&rtcp.ReceiverReport{})
	if got := m.Snapshot().PacketsLost; got != 13 {
		t.Fatalf("PacketsLost after empty report = %d, want 13", got)
	}
}

// TestStreamMetricsLossNegativeClamped 钉住 RFC 3550 的 24-bit signed 语义：
// TotalLost 高位置 1 表示负丢包（重复包），pion 按 uint32 解码后是 ~1677 万，
// 这类报告块必须被忽略，不能计入 PacketsLost；正常正值报告仍需生效。
func TestStreamMetricsLossNegativeClamped(t *testing.T) {
	m := &StreamMetrics{}
	m.ObserveReceiverReport(&rtcp.ReceiverReport{
		Reports: []rtcp.ReceptionReport{{SSRC: 1, TotalLost: 5}},
	})
	if got := m.Snapshot().PacketsLost; got != 5 {
		t.Fatalf("PacketsLost after normal report = %d, want 5", got)
	}

	for _, negative := range []uint32{1 << 23, 1<<24 - 1} {
		m.ObserveReceiverReport(&rtcp.ReceiverReport{
			Reports: []rtcp.ReceptionReport{{SSRC: 1, TotalLost: negative}},
		})
		if got := m.Snapshot().PacketsLost; got != 5 {
			t.Fatalf("PacketsLost after negative TotalLost %d = %d, want 5", negative, got)
		}
	}

	// 上限内的正值（含最大值 1<<23-1）仍应正常累计。
	m.ObserveReceiverReport(&rtcp.ReceiverReport{
		Reports: []rtcp.ReceptionReport{{SSRC: 2, TotalLost: maxReportableLost}},
	})
	if got, want := m.Snapshot().PacketsLost, uint64(5+maxReportableLost); got != want {
		t.Fatalf("PacketsLost after max positive report = %d, want %d", got, want)
	}
}

// TestStreamMetricsLossSSRCCap 钉住 SSRC 追踪上限：异常客户端持续引入新 SSRC 时
// latestLost 不能无界增长；已达上限后未知 SSRC 被忽略，已追踪 SSRC 的更新仍生效。
func TestStreamMetricsLossSSRCCap(t *testing.T) {
	m := &StreamMetrics{}
	for i := uint32(1); i <= maxLostSSRC; i++ {
		m.ObserveReceiverReport(&rtcp.ReceiverReport{
			Reports: []rtcp.ReceptionReport{{SSRC: i, TotalLost: 1}},
		})
	}
	if got := len(m.latestLost); got != maxLostSSRC {
		t.Fatalf("latestLost size = %d, want %d", got, maxLostSSRC)
	}

	// 第 17 个 SSRC：忽略报告块，map 不增长，也不计入 PacketsLost。
	m.ObserveReceiverReport(&rtcp.ReceiverReport{
		Reports: []rtcp.ReceptionReport{{SSRC: 100, TotalLost: 1000}},
	})
	if got := len(m.latestLost); got != maxLostSSRC {
		t.Fatalf("latestLost size after new SSRC = %d, want %d", got, maxLostSSRC)
	}
	if _, ok := m.latestLost[100]; ok {
		t.Fatal("new SSRC should be ignored once latestLost is full")
	}
	if got, want := m.Snapshot().PacketsLost, uint64(maxLostSSRC); got != want {
		t.Fatalf("PacketsLost with ignored SSRC = %d, want %d", got, want)
	}

	// 已追踪 SSRC 的更新不受上限影响。
	m.ObserveReceiverReport(&rtcp.ReceiverReport{
		Reports: []rtcp.ReceptionReport{{SSRC: 1, TotalLost: 7}},
	})
	if got := m.latestLost[1]; got != 7 {
		t.Fatalf("known SSRC update = %d, want 7", got)
	}
	if got, want := m.Snapshot().PacketsLost, uint64(maxLostSSRC-1+7); got != want {
		t.Fatalf("PacketsLost after known SSRC update = %d, want %d", got, want)
	}
}

// TestMultipleConnectionBindsEstimator 钉住 Ruling 4：
// 估计器回调可能早于 Start（metrics 尚未创建）触发，Start 时补绑定；
// 已绑定后重复调用应幂等，且目标码率变化要写入指标。
func TestMultipleConnectionBindsEstimator(t *testing.T) {
	t.Run("estimator arrives before metrics", func(t *testing.T) {
		conn := &MultipleConnection{}
		est := &fakeBandwidthEstimator{target: 1_000_000}

		conn.OnBandwidthEstimate(est)
		if conn.metrics != nil {
			t.Fatal("metrics should stay nil before Start")
		}
		if conn.bandwidthEstimator != est {
			t.Fatal("estimator was not stored before Start")
		}

		conn.metrics = &StreamMetrics{}
		conn.bindEstimator()
		if got := conn.metrics.TargetBitrate.Load(); got != 1_000_000 {
			t.Fatalf("TargetBitrate = %d, want 1000000", got)
		}

		est.fire(2_500_000)
		if got := conn.metrics.TargetBitrate.Load(); got != 2_500_000 {
			t.Fatalf("TargetBitrate after change = %d, want 2500000", got)
		}

		// 幂等：再次绑定不应重复注册回调。
		conn.bindEstimator()
		if got := est.registrationCount(); got != 1 {
			t.Fatalf("OnTargetBitrateChange registered %d times, want 1", got)
		}
	})

	t.Run("estimator arrives after metrics", func(t *testing.T) {
		conn := &MultipleConnection{metrics: &StreamMetrics{}}
		est := &fakeBandwidthEstimator{target: 3_000_000}

		conn.OnBandwidthEstimate(est)
		if got := conn.metrics.TargetBitrate.Load(); got != 3_000_000 {
			t.Fatalf("TargetBitrate = %d, want 3000000", got)
		}
		if got := est.registrationCount(); got != 1 {
			t.Fatalf("OnTargetBitrateChange registered %d times, want 1", got)
		}
	})

	t.Run("nil estimator is ignored", func(t *testing.T) {
		conn := &MultipleConnection{metrics: &StreamMetrics{}}
		conn.OnBandwidthEstimate(nil)
		if conn.bandwidthEstimator != nil {
			t.Fatal("nil estimator should not be stored")
		}
		conn.bindEstimator() // 不应 panic
	})
}
