package m7s

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	cfg "m7s.live/v5/pkg/config"
)

const (
	// apiRouteWaitStreamInterval 是等待流上线时的轮询间隔。
	// 取值在“发现延迟”与“跨节点 StreamInfo 探测压力”之间折中。
	apiRouteWaitStreamInterval = 200 * time.Millisecond
)

// apiRouteWaitMethods 记录允许在路由层等待流上线的 gRPC 方法。
// 只有显式注册的方法才会进入等待；未注册方法保持原有的一次性探测行为，
// 不会因为集群路由引入任何额外延迟。
var apiRouteWaitMethods sync.Map // map[string]struct{}

// RegisterAPIRouteWaitStream 声明某个可路由方法允许“流未就绪时等待”。
//
// 由插件在 init 中注册（例如 mp4 插件的 /mp4.api/StartRecord），核心包因此
// 不需要 import 任何 plugin，也无需感知插件自身的 waitforpublisher 配置。
// 调用方应同时用 RegisterAPIRouteUnary 注册响应工厂，否则无法转发。
func RegisterAPIRouteWaitStream(fullMethod string) {
	if fullMethod == "" {
		return
	}
	apiRouteWaitMethods.Store(fullMethod, struct{}{})
}

// IsAPIRouteWaitStreamMethod 返回方法是否已注册为“可等待流上线”。
func IsAPIRouteWaitStreamMethod(fullMethod string) bool {
	_, ok := apiRouteWaitMethods.Load(fullMethod)
	return ok
}

// apiRouteShouldWaitStream 判断本次请求是否进入等待窗口：
// 方法已注册且窗口大于 0。纯函数，便于单测。
func apiRouteShouldWaitStream(fullMethod string, waitTimeout time.Duration) bool {
	return waitTimeout > 0 && IsAPIRouteWaitStreamMethod(fullMethod)
}

// apiRouteForwardTimeout 返回向目标节点转发时的 RPC 超时。
//
// 等待类方法在目标节点上可能还要执行插件自身的等待（例如 mp4 的
// record.waitforpublisher，默认窗口与 waitstreamtimeout 相同）。如果转发仍只用
// ForwardTimeout，目标节点的等待尚未结束调用方就可能先超时。因此对等待类方法
// 叠加一个等待窗口：转发 RPC 超时 = ForwardTimeout + WaitStreamTimeout。
// ForwardTimeout<=0 表示不限制（沿用 forwardUnary 的语义），不叠加。
func apiRouteForwardTimeout(conf cfg.APIRoute, fullMethod string) time.Duration {
	if conf.ForwardTimeout <= 0 {
		return 0
	}
	if apiRouteShouldWaitStream(fullMethod, conf.WaitStreamTimeout) {
		return conf.ForwardTimeout + conf.WaitStreamTimeout
	}
	return conf.ForwardTimeout
}

// apiRouteWaitAction 表示等待窗口内一次探测后的下一步动作。
type apiRouteWaitAction int

const (
	// apiRouteWaitContinue 尚未发现流，继续等待。
	apiRouteWaitContinue apiRouteWaitAction = iota
	// apiRouteWaitLocal 流已出现在本机，直接本地执行。
	apiRouteWaitLocal
	// apiRouteWaitForward 流已出现在 peer，转发到该节点。
	apiRouteWaitForward
	// apiRouteWaitNotFound 窗口用尽仍没有流，返回 404。
	apiRouteWaitNotFound
)

// apiRouteWaitProbe 是一次探测结果。local 优先于 target：本机已有流时即使同时
// 探测到 peer，也应本地执行，避免把本机请求绕一圈转发出去。
type apiRouteWaitProbe struct {
	local  bool
	target string
}

// decideAPIRouteWaitAction 根据探测结果和窗口是否用尽决定下一步，是纯函数。
// 判定顺序固定为：本地 > peer > 超时，保证本地流优先且不会被探测延迟拖后。
func decideAPIRouteWaitAction(probe apiRouteWaitProbe, expired bool) (apiRouteWaitAction, string) {
	switch {
	case probe.local:
		return apiRouteWaitLocal, ""
	case probe.target != "":
		return apiRouteWaitForward, probe.target
	case expired:
		return apiRouteWaitNotFound, ""
	default:
		return apiRouteWaitContinue, ""
	}
}

// waitAPIRouteStream 在 timeout 窗口内轮询 probe，直到流出现在本地/peer 或窗口用尽。
// 每次探测都带一个不晚于窗口截止时间的 ctx，避免单次跨节点探测拖长总阻塞。
// 等待发生在调用方 goroutine（gRPC handler）上，只用定时器，不创建裸 goroutine。
func waitAPIRouteStream(ctx context.Context, timeout, interval time.Duration,
	probe func(context.Context) apiRouteWaitProbe) (apiRouteWaitAction, string) {
	if timeout <= 0 {
		return apiRouteWaitNotFound, ""
	}
	if interval <= 0 {
		interval = apiRouteWaitStreamInterval
	}
	// 窗口小于探测间隔时收紧间隔，保证小窗口下也能及时探测。
	if interval > timeout {
		interval = timeout
	}
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		probeCtx, cancel := context.WithDeadline(ctx, deadline)
		result := probe(probeCtx)
		cancel()
		expired := !time.Now().Before(deadline)
		if action, target := decideAPIRouteWaitAction(result, expired); action != apiRouteWaitContinue {
			return action, target
		}
		select {
		case <-ctx.Done():
			return apiRouteWaitNotFound, ""
		case <-ticker.C:
		}
	}
}

// routeUnaryWithStreamWait 处理等待类方法的路由：本地优先，其次 peer，窗口用尽 404。
// 探测复用 Streams.SafeGet 与带截止时间的 StreamInfo（resolveStreamTarget），
// 不阻塞 Streams 事件循环，也不创建裸 goroutine。
func (s *Server) routeUnaryWithStreamWait(ctx context.Context, req any, info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler, router *apiRouter, peers []string, conf cfg.APIRoute, streamPath string) (any, error) {
	localListenAddr := s.GetCommonConf().TCP.ListenAddr
	start := time.Now()
	s.Info("api route waits for stream to be published",
		"method", info.FullMethod, "streamPath", streamPath, "timeout", conf.WaitStreamTimeout)
	probe := func(probeCtx context.Context) apiRouteWaitProbe {
		if _, exists := s.Streams.SafeGet(streamPath); exists {
			return apiRouteWaitProbe{local: true}
		}
		target, err := router.resolveStreamTarget(probeCtx, streamPath, peers, conf.ResolveTimeout)
		if err != nil || target == "" || apiRouteIsSelfPeer(localListenAddr, target) {
			return apiRouteWaitProbe{}
		}
		return apiRouteWaitProbe{target: target}
	}
	switch action, target := waitAPIRouteStream(ctx, conf.WaitStreamTimeout, apiRouteWaitStreamInterval, probe); action {
	case apiRouteWaitLocal:
		s.Info("api route stream appeared locally",
			"method", info.FullMethod, "streamPath", streamPath, "waited", time.Since(start))
		return handler(ctx, req)
	case apiRouteWaitForward:
		router.cacheSet(streamPath, target, conf.CacheTTL, time.Now())
		s.Info("api route stream appeared on peer, forwarding",
			"method", info.FullMethod, "streamPath", streamPath, "target", target, "waited", time.Since(start))
		return router.forwardUnary(ctx, info.FullMethod, req, target, apiRouteForwardTimeout(conf, info.FullMethod))
	default:
		s.Warn("api route wait for stream timed out",
			"method", info.FullMethod, "streamPath", streamPath,
			"timeout", conf.WaitStreamTimeout, "waited", time.Since(start))
		return nil, status.Errorf(codes.NotFound, "stream not found in cluster: %s", streamPath)
	}
}
