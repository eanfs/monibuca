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

	line := snap.LogLine()
	for _, key := range []string{
		"target_bitrate=2000000",
		"send_bitrate=1500000",
		"packets_sent=2",
		"packets_lost=0",
		"pli_requests=3",
	} {
		if !strings.Contains(line, key) {
			t.Errorf("LogLine() = %q, missing %q", line, key)
		}
	}
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
