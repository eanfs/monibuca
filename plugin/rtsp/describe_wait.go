package plugin_rtsp

import (
	"context"
	"errors"
	"net/http"
	"time"

	"m7s.live/v5/pkg"
	"m7s.live/v5/pkg/config"
)

// describeSubscribeConfig 用 RTSP 播放的轨道等待上限收紧订阅配置。
// 只收紧不放大：全局 subscribe.waittimeout 更短时保持全局值；
// waitTrackTimeout<=0 表示沿用全局配置。
func describeSubscribeConfig(base config.Subscribe, waitTrackTimeout time.Duration) config.Subscribe {
	if waitTrackTimeout > 0 && (base.WaitTimeout <= 0 || base.WaitTimeout > waitTrackTimeout) {
		base.WaitTimeout = waitTrackTimeout
	}
	return base
}

// describeErrorResponse 把 RTSP 播放订阅阶段的错误映射为 RTSP 状态码：
//   - 轨道未就绪 / 等待超时（publisher 在但无 track、摄像头重连等）是临时状态，
//     返回 503 + Retry-After，让客户端尽快重试而不是挂到自己的打开超时；
//   - 流不存在返回 404；
//   - 其它错误保持原有 400（状态文本保留具体原因）。
func describeErrorResponse(err error) (statusCode int, statusText string, retryAfterSeconds int) {
	switch {
	case errors.Is(err, pkg.ErrSubscribeTimeout),
		errors.Is(err, pkg.ErrNoTrack),
		errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable, http.StatusText(http.StatusServiceUnavailable), 1
	case errors.Is(err, pkg.ErrNotFound):
		return http.StatusNotFound, http.StatusText(http.StatusNotFound), 0
	default:
		return http.StatusBadRequest, err.Error(), 0
	}
}
