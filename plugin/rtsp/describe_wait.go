package plugin_rtsp

import (
	"context"
	"errors"
	"net/http"
	"net/textproto"
	"strconv"
	"time"

	"m7s.live/v5/pkg"
	"m7s.live/v5/pkg/config"
	"m7s.live/v5/pkg/util"
)

// describeRetryAfter 是流/轨道尚未就绪（临时状态）时建议客户端重试的间隔。
const describeRetryAfter = time.Second

// describeSubscribeConfig 用 RTSP 播放的等待上限收紧订阅配置。
// 只收紧不放大：全局值更短时保持全局值；waitTimeout<=0 表示沿用全局配置。
//
// 收紧的是 InitialWaitTimeout（首次订阅的等流 + 等轨道总预算），**不动 WaitTimeout**：
// 后者同时是 subscriber 整个生命周期的重等预算（subscriber.go checkPublishChanged
// 与 server.go 卡死兜底），把它压到几秒会让已经在播放的连接在 publisher 短暂消失
// （如拉流重试间隔 5s）时提前放弃。
func describeSubscribeConfig(base config.Subscribe, waitTimeout time.Duration) config.Subscribe {
	if waitTimeout > 0 && (base.InitialWaitTimeout <= 0 || base.InitialWaitTimeout > waitTimeout) {
		base.InitialWaitTimeout = waitTimeout
	}
	return base
}

// describeFailure 描述 DESCRIBE 订阅失败时应回给客户端的 RTSP 响应。
// 状态码与重试间隔总是一起决定，收成一个类型避免调用方各自拼装。
type describeFailure struct {
	StatusCode int
	StatusText string
	RetryAfter time.Duration
}

// describeErrorFailure 把订阅阶段的错误映射为 RTSP 响应：
//   - 等流/等轨道超时（publisher 迟迟不出现、publisher 在但轨道未就绪、摄像头重连）
//     都属临时状态 → 503 + Retry-After，让客户端尽快重试而不是挂到自己的打开超时；
//   - publisher 明确不提供所需轨道（永久条件，如纯音频流等 video）或其它错误 → 400。
//
// 这里刻意不用 404：拉流代理 / 跨节点 relay 是异步拉起 publisher 的，客户端也常早于
// 推流接入，此时流只是「还没准备好」。404 会让 AI 等客户端按永久失败放弃，正好抵消
// 本策略「快速失败可重试」的目的。
func describeErrorFailure(err error) describeFailure {
	switch {
	case errors.Is(err, pkg.ErrSubscribeTimeout), errors.Is(err, context.DeadlineExceeded):
		return describeFailure{
			StatusCode: http.StatusServiceUnavailable,
			StatusText: http.StatusText(http.StatusServiceUnavailable),
			RetryAfter: describeRetryAfter,
		}
	default:
		// 具体原因只进日志：错误原文回给客户端既无区分度又可能泄露内部信息。
		return describeFailure{
			StatusCode: http.StatusBadRequest,
			StatusText: http.StatusText(http.StatusBadRequest),
		}
	}
}

// response 构造发往客户端的 RTSP 响应。
func (f describeFailure) response(req *util.Request) *util.Response {
	header := textproto.MIMEHeader{}
	if f.RetryAfter > 0 {
		header.Set("Retry-After", strconv.Itoa(int(f.RetryAfter/time.Second)))
	}
	return &util.Response{
		StatusCode: f.StatusCode,
		Status:     f.StatusText,
		Header:     header,
		Request:    req,
	}
}
