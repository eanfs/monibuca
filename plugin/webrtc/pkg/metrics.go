package webrtc

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/pion/rtcp"
)

const (
	// maxLostSSRC 限制单个连接内追踪的 ReceiverReport SSRC 数量。
	// SSRC 来自对端的 RTCP 报告，异常客户端可以在一个连接里持续引入新 SSRC；
	// 正常流（1 路视频 + 1 路音频）只有 1–2 个，16 已足够宽裕。
	// 达到上限后只更新已追踪的 SSRC，未知 SSRC 的新报告块直接忽略，保证内存有界。
	maxLostSSRC = 16
	// maxReportableLost 是 RFC 3550 中 TotalLost 作为 24-bit signed 的上限。
	// 超过它的值（最高位为 1）实际表示负丢包（重复包），pion 按 uint32 解码后
	// 会变成约 1677 万的假丢包，必须丢弃而不是计入 PacketsLost。
	maxReportableLost = 1<<23 - 1
)

// StreamMetrics 汇总单个 WebRTC 连接（MultipleConnection）的发送端观测指标。
// 所有方法都是 nil 安全的：埋点路径可能早于/脱离 metrics 初始化。
type StreamMetrics struct {
	PacketsSent   atomic.Uint64
	BytesSent     atomic.Uint64
	PacketsLost   atomic.Uint64
	PLIRequests   atomic.Uint64
	TargetBitrate atomic.Int64
	SendBitrate   atomic.Int64
	// RTT/Jitter 来自发送侧 PC 的 RemoteInboundRTPStreamStats，单位毫秒。
	RTT    atomic.Int64
	Jitter atomic.Int64

	// lostMu 保护 latestLost；ReceiverReport.TotalLost 是累计值，
	// 同一 SSRC 只保留最新一次报告的数值，避免重复累加。
	lostMu     sync.Mutex
	latestLost map[uint32]uint32
}

// MetricsSnapshot 是某一时刻指标的只读快照。
type MetricsSnapshot struct {
	PacketsSent   uint64
	BytesSent     uint64
	PacketsLost   uint64
	PLIRequests   uint64
	TargetBitrate int64
	SendBitrate   int64
	RTT           int64
	Jitter        int64
}

// AddSent 记录一个成功写出的 RTP 包。
func (m *StreamMetrics) AddSent(payloadBytes int) {
	if m == nil {
		return
	}
	m.PacketsSent.Add(1)
	if payloadBytes > 0 {
		m.BytesSent.Add(uint64(payloadBytes))
	}
}

// ObserveReceiverReport 消费对端发来的 RTCP ReceiverReport。
// Ruling 6：TotalLost 已经是自接收开始以来的累计丢包数，
// 因此每个 SSRC 只记录最新值，PacketsLost 为各 SSRC 最新值之和。
func (m *StreamMetrics) ObserveReceiverReport(report *rtcp.ReceiverReport) {
	if m == nil || report == nil {
		return
	}
	m.lostMu.Lock()
	defer m.lostMu.Unlock()
	if m.latestLost == nil {
		m.latestLost = make(map[uint32]uint32, len(report.Reports))
	}
	for _, rr := range report.Reports {
		// RFC 3550：24-bit signed，超过上限的值表示负丢包（重复包），
		// 不能当作丢包累加（否则日志会出现约 1677 万的假丢包）。
		if rr.TotalLost > maxReportableLost {
			continue
		}
		if _, known := m.latestLost[rr.SSRC]; !known && len(m.latestLost) >= maxLostSSRC {
			// 未知 SSRC 且已达上限：忽略该报告块，避免对端伪造 SSRC 撑爆内存。
			continue
		}
		m.latestLost[rr.SSRC] = rr.TotalLost
	}
	var total uint64
	for _, lost := range m.latestLost {
		total += uint64(lost)
	}
	m.PacketsLost.Store(total)
}

// Snapshot 返回当前指标快照。
func (m *StreamMetrics) Snapshot() MetricsSnapshot {
	if m == nil {
		return MetricsSnapshot{}
	}
	return MetricsSnapshot{
		PacketsSent:   m.PacketsSent.Load(),
		BytesSent:     m.BytesSent.Load(),
		PacketsLost:   m.PacketsLost.Load(),
		PLIRequests:   m.PLIRequests.Load(),
		TargetBitrate: m.TargetBitrate.Load(),
		SendBitrate:   m.SendBitrate.Load(),
		RTT:           m.RTT.Load(),
		Jitter:        m.Jitter.Load(),
	}
}

// LogLine 输出单行 key=value 形式的指标，便于日志检索。
func (s MetricsSnapshot) LogLine() string {
	return fmt.Sprintf(
		"target_bitrate=%d send_bitrate=%d packets_sent=%d packets_lost=%d pli_requests=%d rtt=%d jitter=%d",
		s.TargetBitrate, s.SendBitrate, s.PacketsSent, s.PacketsLost, s.PLIRequests, s.RTT, s.Jitter,
	)
}
