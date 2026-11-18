package webrtc

import (
	"sync"
	"time"

	"github.com/pion/rtcp"
	"m7s.live/v5"
)

const (
	// defaultPLIMinInterval 是同一 Publisher 两次 PLI 之间的最小间隔。
	defaultPLIMinInterval = 500 * time.Millisecond
	// defaultPLIFallback 是长时间收不到关键帧时的兜底放行窗口。
	defaultPLIFallback = 5 * time.Second
	// keyFrameWaitLimit 是推流端主动请求关键帧的等待上限：
	// 超过这么久没有新的 IDR，就按默认节流预算向上游要一帧。
	keyFrameWaitLimit = 5 * time.Second
	// severeLossThreshold 是 RTCP FractionLost 的严重丢包阈值。
	// FractionLost 以 1/256 为单位，25/256 ≈ 9.8%，与 10% 等价。
	severeLossThreshold uint8 = 25
)

// PLIThrottler 限制同一个 Publisher 上的 PLI（关键帧请求）速率。
//
// 节流状态属于 WebRTC 推流端的 MultipleConnection，而不是每个观众连接：
// 所有观众与推流端自身的兜底检测共用同一个实例，因此 N 个观众不会把
// PLI 速率放大 N 倍（Ruling 7）。
type PLIThrottler struct {
	MinInterval time.Duration
	Fallback    time.Duration

	mu       sync.Mutex
	lastSent time.Time
}

// NewPLIThrottler 创建 PLI 节流器。minInterval 是两次请求的最小间隔，
// fallback 是“距上次关键帧已过多久可无视最小间隔放行”的兜底窗口。
func NewPLIThrottler(minInterval, fallback time.Duration) *PLIThrottler {
	return &PLIThrottler{MinInterval: minInterval, Fallback: fallback}
}

// Allow 判断此刻是否允许发送一次 PLI，允许时记录本次发送时间。
// now 为当前时间，lastKeyFrame 为最近一次成功写出的 IDR 帧时间。
//
// 规则：
//  1. 距上次发送已超过 MinInterval → 放行；
//  2. 否则若 Fallback > 0 且距上次关键帧已超过 Fallback → 放行；
//  3. 其他情况拒绝。
//
// Fallback 分支仅为 brief 的接口/测试语义保留：生产中的所有请求路径
// （观众 PLI / 严重丢包转发、逐包无 IDR 兜底）都以 Allow(now, now) 调用，
// 只受 MinInterval 约束；5s“无 IDR”是调用方的触发条件，不是绕过 500ms
// 硬窗口的旁路。这样保证“同一 Publisher 500ms 内最多一次 PLI”的不变量
// 不会被观众数量或请求频率放大。
//
// 接收者为 nil 时视为“未启用节流”，直接放行（Optional）。
func (p *PLIThrottler) Allow(now, lastKeyFrame time.Time) bool {
	if p == nil {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if now.Sub(p.lastSent) >= p.MinInterval {
		p.lastSent = now
		return true
	}
	if p.Fallback > 0 && now.Sub(lastKeyFrame) >= p.Fallback {
		p.lastSent = now
		return true
	}
	return false
}

// isSevereLoss 判断 RTCP ReceiverReport 是否报告了严重丢包。
// 任一接收块丢失比例 > 10%（25/256）即视为严重，用于触发关键帧请求。
func isSevereLoss(report *rtcp.ReceiverReport) bool {
	if report == nil {
		return false
	}
	for _, rr := range report.Reports {
		if rr.FractionLost > severeLossThreshold {
			return true
		}
	}
	return false
}

// forwardPublisherPLI 把观众侧的 PLI 或严重丢包报告转发给订阅流的推流端，
// 由推流端 WebRTC ingest 连接统一节流后向 Publisher 发送 PLI。
//
// 只有 WebRTC 推流（带 Publisher 的 MultipleConnection）会在
// publisherKeyFrameRequesters 中注册目标。非 WebRTC 的 Publisher（RTMP 推流、
// RTSP 拉流代理等）没有 PLI 通道，查不到目标时直接跳过，不做任何降级处理。
func forwardPublisherPLI(suber *m7s.Subscriber, now time.Time) {
	if suber == nil || suber.Publisher == nil {
		return
	}
	target := lookupPublisherKeyFrameRequester(suber.Publisher)
	if target == nil {
		return
	}
	target.requestKeyFrame(now)
}
