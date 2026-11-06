package m7s

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	task "github.com/eanfs/gotask"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"m7s.live/v5/pb"
	cfg "m7s.live/v5/pkg/config"
	mp4pb "m7s.live/v5/plugin/mp4/pb"
)

// issue #15 回归：集群 APIRoute 下，等待类方法（/mp4.api/StartRecord）在流未发布时
// 应由路由层等待，而不是进入 handler 前直接 404。这里同时钉住纯决策函数与拦截器接线。

func init() {
	// 生产环境中由各插件 init 注册；根包测试不加载插件包，这里等价注册。
	RegisterAPIRouteUnary("/mp4.api/StartRecord", func() any { return new(mp4pb.ResponseStartRecord) })
	RegisterAPIRouteUnary("/mp4.api/StopRecord", func() any { return new(mp4pb.ResponseStopRecord) })
	RegisterAPIRouteWaitStream("/mp4.api/StartRecord")
}

const apiRouteTestNeverAppear int32 = 1 << 30

// apiRouteTestPeer 模拟一个既有 /global.api/StreamInfo 又有 /mp4.api/StartRecord 的 peer。
type apiRouteTestPeer struct {
	pb.UnimplementedApiServer

	addr             string
	appearAfter      atomic.Int32
	streamInfoCalls  atomic.Int32
	startRecordCalls atomic.Int32
	lastStreamPath   atomic.Value
}

func (p *apiRouteTestPeer) StreamInfo(context.Context, *pb.StreamSnapRequest) (*pb.StreamInfoResponse, error) {
	if n := p.streamInfoCalls.Add(1); n < p.appearAfter.Load() {
		return nil, status.Error(codes.NotFound, "stream not found")
	}
	return &pb.StreamInfoResponse{}, nil
}

type apiRouteTestMP4Server struct {
	mp4pb.UnimplementedApiServer
	peer *apiRouteTestPeer
}

func (s *apiRouteTestMP4Server) StartRecord(_ context.Context, req *mp4pb.ReqStartRecord) (*mp4pb.ResponseStartRecord, error) {
	s.peer.startRecordCalls.Add(1)
	s.peer.lastStreamPath.Store(req.GetStreamPath())
	return &mp4pb.ResponseStartRecord{Data: 42}, nil
}

func startAPIRouteTestPeer(t *testing.T, appearAfter int32) *apiRouteTestPeer {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	peer := &apiRouteTestPeer{addr: lis.Addr().String()}
	peer.appearAfter.Store(appearAfter)
	gs := grpc.NewServer()
	pb.RegisterApiServer(gs, peer)
	mp4pb.RegisterApiServer(gs, &apiRouteTestMP4Server{peer: peer})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return peer
}

func newAPIRouteTestServer(t *testing.T, peerAddr string, waitTimeout, forwardTimeout time.Duration) *Server {
	t.Helper()
	s := &Server{}
	s.Logger = slog.New(slog.DiscardHandler)
	s.Plugin.config.APIRoute = cfg.APIRoute{
		Enable:            true,
		Nodes:             []cfg.APIRouteNode{{GRPC: peerAddr}},
		ResolveTimeout:    200 * time.Millisecond,
		ForwardTimeout:    forwardTimeout,
		WaitStreamTimeout: waitTimeout,
	}
	// 启动真实的 Streams 事件循环：SafeGet 会走 Post/Call 而非测试内联路径，
	// 从而验证等待逻辑不会阻塞事件循环。
	var root task.RootManager[uint32, task.ManagerItem[uint32]]
	root.Init()
	if err := root.AddTask(&s.Streams).WaitStarted(); err != nil {
		t.Fatalf("start Streams: %v", err)
	}
	return s
}

func runAPIRouteTestInterceptor(t *testing.T, s *Server, fullMethod string, req any,
	handler grpc.UnaryHandler) (any, error) {
	t.Helper()
	return s.RouteInterceptor()(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: fullMethod}, handler)
}

func TestDecideAPIRouteWaitAction(t *testing.T) {
	cases := []struct {
		name       string
		probe      apiRouteWaitProbe
		expired    bool
		wantAction apiRouteWaitAction
		wantTarget string
	}{
		{"本地流优先", apiRouteWaitProbe{local: true, target: "peer:1"}, false, apiRouteWaitLocal, ""},
		{"peer 出现", apiRouteWaitProbe{target: "peer:1"}, false, apiRouteWaitForward, "peer:1"},
		{"窗口内继续等待", apiRouteWaitProbe{}, false, apiRouteWaitContinue, ""},
		{"窗口用尽", apiRouteWaitProbe{}, true, apiRouteWaitNotFound, ""},
		{"窗口用尽但本地刚出现", apiRouteWaitProbe{local: true}, true, apiRouteWaitLocal, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			action, target := decideAPIRouteWaitAction(c.probe, c.expired)
			if action != c.wantAction || target != c.wantTarget {
				t.Fatalf("decideAPIRouteWaitAction(%+v, %v) = (%v, %q), want (%v, %q)",
					c.probe, c.expired, action, target, c.wantAction, c.wantTarget)
			}
		})
	}
}

func TestAPIRouteShouldWaitStream(t *testing.T) {
	const waitMethod = "/test.api/WaitStream"
	RegisterAPIRouteWaitStream(waitMethod)
	t.Cleanup(func() { apiRouteWaitMethods.Delete(waitMethod) })

	if !apiRouteShouldWaitStream(waitMethod, 10*time.Second) {
		t.Fatal("registered method with positive timeout should wait")
	}
	if apiRouteShouldWaitStream(waitMethod, 0) {
		t.Fatal("zero timeout must not wait")
	}
	// 未注册的方法（如 StopRecord）不允许等待，避免给其它 API 引入额外延迟。
	if apiRouteShouldWaitStream("/mp4.api/StopRecord", 10*time.Second) {
		t.Fatal("unregistered method must not wait")
	}
}

func TestWaitAPIRouteStreamLocalImmediately(t *testing.T) {
	var calls atomic.Int32
	start := time.Now()
	action, target := waitAPIRouteStream(context.Background(), 5*time.Second, 10*time.Millisecond,
		func(context.Context) apiRouteWaitProbe {
			calls.Add(1)
			return apiRouteWaitProbe{local: true}
		})
	if action != apiRouteWaitLocal || target != "" {
		t.Fatalf("waitAPIRouteStream() = (%v, %q), want local", action, target)
	}
	if calls.Load() != 1 {
		t.Fatalf("probe called %d times, want 1", calls.Load())
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("local probe took %v, should return immediately", elapsed)
	}
}

func TestWaitAPIRouteStreamPeerAppearsWithinWindow(t *testing.T) {
	var calls atomic.Int32
	action, target := waitAPIRouteStream(context.Background(), 2*time.Second, 5*time.Millisecond,
		func(context.Context) apiRouteWaitProbe {
			if calls.Add(1) < 3 {
				return apiRouteWaitProbe{}
			}
			return apiRouteWaitProbe{target: "peer:1"}
		})
	if action != apiRouteWaitForward || target != "peer:1" {
		t.Fatalf("waitAPIRouteStream() = (%v, %q), want forward to peer:1", action, target)
	}
	if calls.Load() < 3 {
		t.Fatalf("probe called %d times, want >= 3", calls.Load())
	}
}

func TestWaitAPIRouteStreamWindowExpires(t *testing.T) {
	start := time.Now()
	action, target := waitAPIRouteStream(context.Background(), 40*time.Millisecond, 5*time.Millisecond,
		func(context.Context) apiRouteWaitProbe { return apiRouteWaitProbe{} })
	elapsed := time.Since(start)
	if action != apiRouteWaitNotFound || target != "" {
		t.Fatalf("waitAPIRouteStream() = (%v, %q), want not found", action, target)
	}
	if elapsed < 40*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("waitAPIRouteStream() returned after %v, want about 40ms", elapsed)
	}
}

func TestAPIRouteForwardTimeout(t *testing.T) {
	const waitMethod = "/test.api/WaitForward"
	RegisterAPIRouteWaitStream(waitMethod)
	t.Cleanup(func() { apiRouteWaitMethods.Delete(waitMethod) })

	conf := cfg.APIRoute{ForwardTimeout: 10 * time.Second, WaitStreamTimeout: 10 * time.Second}
	if got := apiRouteForwardTimeout(conf, waitMethod); got != 20*time.Second {
		t.Fatalf("wait method forward timeout = %v, want 20s (forward + wait window)", got)
	}
	if got := apiRouteForwardTimeout(conf, "/mp4.api/StopRecord"); got != 10*time.Second {
		t.Fatalf("non-wait method forward timeout = %v, want 10s", got)
	}
	conf.ForwardTimeout = 0
	if got := apiRouteForwardTimeout(conf, waitMethod); got != 0 {
		t.Fatalf("forward timeout 0 means unlimited, got %v", got)
	}
}

// 本地已有流时不应等待，也不应探测 peer。
func TestRouteInterceptorLocalStreamExecutesImmediately(t *testing.T) {
	peer := startAPIRouteTestPeer(t, apiRouteTestNeverAppear)
	s := newAPIRouteTestServer(t, peer.addr, 5*time.Second, time.Second)
	pub := &Publisher{}
	pub.StreamPath = "live/local"
	s.Streams.Collection.Add(pub)

	var localCalls atomic.Int32
	start := time.Now()
	res, err := runAPIRouteTestInterceptor(t, s, "/mp4.api/StartRecord",
		&mp4pb.ReqStartRecord{StreamPath: "live/local"},
		func(context.Context, any) (any, error) {
			localCalls.Add(1)
			return &mp4pb.ResponseStartRecord{Data: 7}, nil
		})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RouteInterceptor() error = %v", err)
	}
	if localCalls.Load() != 1 {
		t.Fatalf("local handler called %d times, want 1", localCalls.Load())
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("local routing took %v, should not wait", elapsed)
	}
	if got := peer.streamInfoCalls.Load(); got != 0 {
		t.Fatalf("peer probed %d times for a local stream, want 0", got)
	}
	if got := peer.startRecordCalls.Load(); got != 0 {
		t.Fatalf("peer forwarded %d times for a local stream, want 0", got)
	}
	if got := res.(*mp4pb.ResponseStartRecord).GetData(); got != 7 {
		t.Fatalf("local handler response data = %d, want 7", got)
	}
}

// peer 在等待窗口内出现时应转发，而不是 404。
func TestRouteInterceptorStartRecordWaitsForPeerThenForwards(t *testing.T) {
	peer := startAPIRouteTestPeer(t, 3)
	s := newAPIRouteTestServer(t, peer.addr, 3*time.Second, time.Second)

	var localCalls atomic.Int32
	start := time.Now()
	res, err := runAPIRouteTestInterceptor(t, s, "/mp4.api/StartRecord",
		&mp4pb.ReqStartRecord{StreamPath: "live/cam"},
		func(context.Context, any) (any, error) {
			localCalls.Add(1)
			return &mp4pb.ResponseStartRecord{Data: 7}, nil
		})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RouteInterceptor() error = %v, want forwarded success", err)
	}
	if localCalls.Load() != 0 {
		t.Fatalf("local handler called %d times, want 0 (should forward)", localCalls.Load())
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("forward took %v, exceeded wait window", elapsed)
	}
	if got := peer.startRecordCalls.Load(); got != 1 {
		t.Fatalf("peer StartRecord called %d times, want 1", got)
	}
	if got, _ := peer.lastStreamPath.Load().(string); got != "live/cam" {
		t.Fatalf("forwarded streamPath = %q, want live/cam", got)
	}
	if got := res.(*mp4pb.ResponseStartRecord).GetData(); got != 42 {
		t.Fatalf("forwarded response data = %d, want 42", got)
	}
	if got := peer.streamInfoCalls.Load(); got < 3 {
		t.Fatalf("peer StreamInfo calls = %d, want >= 3 (must keep probing)", got)
	}
}

// 窗口用尽仍无流 → 404。
func TestRouteInterceptorStartRecordWaitWindowExpires(t *testing.T) {
	peer := startAPIRouteTestPeer(t, apiRouteTestNeverAppear)
	s := newAPIRouteTestServer(t, peer.addr, 150*time.Millisecond, time.Second)

	var localCalls atomic.Int32
	start := time.Now()
	_, err := runAPIRouteTestInterceptor(t, s, "/mp4.api/StartRecord",
		&mp4pb.ReqStartRecord{StreamPath: "live/missing"},
		func(context.Context, any) (any, error) {
			localCalls.Add(1)
			return &mp4pb.ResponseStartRecord{}, nil
		})
	elapsed := time.Since(start)
	if code := status.Code(err); code != codes.NotFound {
		t.Fatalf("RouteInterceptor() code = %v, want NotFound (err=%v)", code, err)
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("RouteInterceptor() returned in %v, wait window not applied", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("RouteInterceptor() took %v, wait window exceeded bound", elapsed)
	}
	if localCalls.Load() != 0 {
		t.Fatalf("local handler called %d times, want 0", localCalls.Load())
	}
	if got := peer.startRecordCalls.Load(); got != 0 {
		t.Fatalf("peer StartRecord called %d times, want 0", got)
	}
}

// 非 StartRecord 方法不得等待：保持原逻辑（探测一次后 404），不引入额外延迟。
func TestRouteInterceptorNonWaitMethodDoesNotWait(t *testing.T) {
	peer := startAPIRouteTestPeer(t, apiRouteTestNeverAppear)
	s := newAPIRouteTestServer(t, peer.addr, 5*time.Second, time.Second)

	var localCalls atomic.Int32
	start := time.Now()
	_, err := runAPIRouteTestInterceptor(t, s, "/mp4.api/StopRecord",
		&mp4pb.ReqStopRecord{StreamPath: "live/missing"},
		func(context.Context, any) (any, error) {
			localCalls.Add(1)
			return &mp4pb.ResponseStopRecord{}, nil
		})
	elapsed := time.Since(start)
	if code := status.Code(err); code != codes.NotFound {
		t.Fatalf("RouteInterceptor() code = %v, want NotFound (err=%v)", code, err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("non-wait method took %v, must not enter wait window", elapsed)
	}
	if localCalls.Load() != 0 {
		t.Fatalf("local handler called %d times, want 0", localCalls.Load())
	}
	if got := peer.streamInfoCalls.Load(); got > 1 {
		t.Fatalf("non-wait method probed peer %d times, want <= 1", got)
	}
}

// 单机（APIRoute 未启用）行为与现状一致：直接进入本地 handler，不等待。
func TestRouteInterceptorAPIRouteDisabledSkipsWait(t *testing.T) {
	peer := startAPIRouteTestPeer(t, apiRouteTestNeverAppear)
	s := newAPIRouteTestServer(t, peer.addr, 5*time.Second, time.Second)
	s.Plugin.config.APIRoute.Enable = false

	var localCalls atomic.Int32
	start := time.Now()
	_, err := runAPIRouteTestInterceptor(t, s, "/mp4.api/StartRecord",
		&mp4pb.ReqStartRecord{StreamPath: "live/missing"},
		func(context.Context, any) (any, error) {
			localCalls.Add(1)
			return &mp4pb.ResponseStartRecord{}, nil
		})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RouteInterceptor() error = %v", err)
	}
	if localCalls.Load() != 1 {
		t.Fatalf("local handler called %d times, want 1", localCalls.Load())
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("disabled APIRoute took %v, should not wait", elapsed)
	}
	if got := peer.streamInfoCalls.Load(); got != 0 {
		t.Fatalf("peer probed %d times when APIRoute disabled, want 0", got)
	}
}

// 流在等待窗口内出现在本机时应本地执行（AC1 的“本地出现→本地执行”），
// 而不是继续等 peer、或等窗口用尽后 404。
func TestRouteInterceptorLocalStreamAppearsWithinWindow(t *testing.T) {
	peer := startAPIRouteTestPeer(t, apiRouteTestNeverAppear)
	s := newAPIRouteTestServer(t, peer.addr, 3*time.Second, time.Second)

	// 该用例从另一个 goroutine 模拟 publisher 上线，需要 Collection 的读写走
	// RWMutex：Streams 在测试里没有子任务，Job.Call 对 Size==0 的调用是内联执行，
	// 并不串行（生产环境 Streams 有子任务时由事件循环串行）。
	var mu sync.RWMutex
	s.Streams.Collection.L = &mu

	pub := &Publisher{}
	pub.StreamPath = "live/late"
	appeared := make(chan struct{})
	go func() {
		defer close(appeared)
		time.Sleep(300 * time.Millisecond)
		s.Streams.Collection.Add(pub)
	}()
	defer func() { <-appeared }()

	var localCalls atomic.Int32
	start := time.Now()
	res, err := runAPIRouteTestInterceptor(t, s, "/mp4.api/StartRecord",
		&mp4pb.ReqStartRecord{StreamPath: "live/late"},
		func(context.Context, any) (any, error) {
			localCalls.Add(1)
			return &mp4pb.ResponseStartRecord{Data: 11}, nil
		})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RouteInterceptor() error = %v", err)
	}
	if localCalls.Load() != 1 {
		t.Fatalf("local handler called %d times, want 1", localCalls.Load())
	}
	if elapsed < 250*time.Millisecond {
		t.Fatalf("RouteInterceptor() returned in %v, should wait for the stream", elapsed)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("RouteInterceptor() took %v, should execute locally within the window", elapsed)
	}
	if got := res.(*mp4pb.ResponseStartRecord).GetData(); got != 11 {
		t.Fatalf("local handler response data = %d, want 11", got)
	}
	if got := peer.startRecordCalls.Load(); got != 0 {
		t.Fatalf("peer forwarded %d times, want 0 (stream is local)", got)
	}
}
