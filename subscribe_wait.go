package m7s

import (
	"context"
	"errors"
	"strings"
	"time"

	. "m7s.live/v5/pkg"
	"m7s.live/v5/pkg/config"
)

// defaultSubscribeTimeout 与 config.Subscribe.WaitTimeout 的默认值保持一致，
// 用于配置里写 0（未设置）时兜底。
const defaultSubscribeTimeout = 10 * time.Second

// resolveSubscribeTimeout 计算「首次订阅」的等待预算：等 publisher 上线与等轨道
// 就绪共用这一个预算，总时长不会超过返回值。
//
// 只收紧不放大：InitialWaitTimeout 只在比 WaitTimeout 更短时生效。
// 关键点是**不修改 conf.WaitTimeout 本身**——订阅失败后 caller 仍会把原始 conf
// 留在 subscriber 上，而 subscriber 整个生命周期的重等预算（subscriber.go 的
// checkPublishChanged、server.go 的卡死兜底）读的就是这个字段。把播放期预算
// 压到几秒，会让已经在播放的连接在 publisher 短暂消失（拉流 retryinterval 默认
// 5s）时提前放弃。
func resolveSubscribeTimeout(conf config.Subscribe) time.Duration {
	timeout := conf.WaitTimeout
	if conf.InitialWaitTimeout > 0 && (timeout <= 0 || conf.InitialWaitTimeout < timeout) {
		timeout = conf.InitialWaitTimeout
	}
	if timeout <= 0 {
		timeout = defaultSubscribeTimeout
	}
	return timeout
}

// waitPublisherTracks 在 budget 内等待 publisher 的轨道就绪。
//
//   - budget 已耗尽（<=0）说明等待预算被等 publisher 的阶段用光了，按超时处理；
//   - ctx 被调用方取消时原样返回 context.Canceled，避免把「调用方主动放弃」
//     误判成「流未就绪」而给客户端可重试的信号；
//   - publisher 在等待期间被停止时，轨道就绪 promise 会带着停止原因被取消，
//     WaitTrack 会把它归一成 ErrNoTrack；这不是「不提供该轨道」的永久条件，
//     按超时返回，让客户端重试。
func waitPublisherTracks(ctx context.Context, publisher *Publisher, waitTrack string, budget time.Duration) error {
	if budget <= 0 {
		return ErrSubscribeTimeout
	}
	waitAudio := waitTrack == "all" || strings.Contains(waitTrack, "audio")
	waitVideo := waitTrack == "all" || strings.Contains(waitTrack, "video")
	trackCtx, cancelTrack := context.WithTimeout(ctx, budget)
	defer cancelTrack()
	err := publisher.WaitTrackContext(trackCtx, waitAudio, waitVideo)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return ErrSubscribeTimeout
	case errors.Is(err, ErrNoTrack) && PublisherStopped(publisher):
		return ErrSubscribeTimeout
	}
	return err
}
