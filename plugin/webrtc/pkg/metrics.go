package webrtc

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/pion/rtcp"
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
	}
}

// LogLine 输出单行 key=value 形式的指标，便于日志检索。
func (s MetricsSnapshot) LogLine() string {
	return fmt.Sprintf(
		"target_bitrate=%d send_bitrate=%d packets_sent=%d packets_lost=%d pli_requests=%d",
		s.TargetBitrate, s.SendBitrate, s.PacketsSent, s.PacketsLost, s.PLIRequests,
	)
}
