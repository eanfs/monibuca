package m7s

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"m7s.live/v5/pb"
	cfg "m7s.live/v5/pkg/config"
)

const apiRouteForwardedMetaKey = "x-m7s-api-routed"

type apiRouteCacheEntry struct {
	target string
	expAt  time.Time
}

// apiRouter routes selected gRPC calls to the node that actually hosts the stream.
// It is intended for "control-plane" APIs (recording, snapshot, etc.), not for media-plane traffic.
type apiRouter struct {
	s *Server

	mu    sync.Mutex
	cache map[string]apiRouteCacheEntry

	conns sync.Map // map[string]*grpc.ClientConn
}

func (r *apiRouter) getConn(ctx context.Context, target string) (*grpc.ClientConn, error) {
	if v, ok := r.conns.Load(target); ok {
		return v.(*grpc.ClientConn), nil
	}
	conn, err := grpc.DialContext(ctx, target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	actual, loaded := r.conns.LoadOrStore(target, conn)
	if loaded {
		_ = conn.Close()
		return actual.(*grpc.ClientConn), nil
	}
	r.s.Using(conn)
	return conn, nil
}

func (r *apiRouter) cacheGet(streamPath string, now time.Time) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil {
		r.cache = make(map[string]apiRouteCacheEntry)
		return "", false
	}
	ent, ok := r.cache[streamPath]
	if !ok || ent.target == "" {
		return "", false
	}
	if !ent.expAt.IsZero() && now.After(ent.expAt) {
		delete(r.cache, streamPath)
		return "", false
	}
	return ent.target, true
}

func (r *apiRouter) cacheSet(streamPath, target string, ttl time.Duration, now time.Time) {
	if ttl <= 0 || streamPath == "" || target == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil {
		r.cache = make(map[string]apiRouteCacheEntry)
	}
	r.cache[streamPath] = apiRouteCacheEntry{target: target, expAt: now.Add(ttl)}
}

func normalizePort(addr string) string {
	if addr == "" {
		return ""
	}
	if strings.HasPrefix(addr, ":") {
		return strings.TrimPrefix(addr, ":")
	}
	host, port, err := net.SplitHostPort(addr)
	_ = host
	if err == nil && port != "" {
		return port
	}
	// Best effort: allow "host:port" without brackets, or raw ":port" already handled.
	if i := strings.LastIndex(addr, ":"); i >= 0 && i < len(addr)-1 {
		return addr[i+1:]
	}
	return ""
}

func isSameGRPCEndpoint(localListenAddr, peer string) bool {
	// Deprecated: port-only compare breaks when different hosts share the same port.
	// Kept for backward compatibility with older code paths; prefer apiRouteIsSelfPeer.
	return apiRouteIsSelfPeer(localListenAddr, peer)
}

func extractStreamPath(req any) (string, bool) {
	// We intentionally keep this small and strict for safety:
	// route only when requests carry a StreamPath field.
	type hasStreamPath interface{ GetStreamPath() string }
	if v, ok := req.(hasStreamPath); ok {
		if sp := v.GetStreamPath(); sp != "" {
			return sp, true
		}
	}
	return "", false
}

func (s *Server) apiRouter() *apiRouter {
	if s == nil {
		return nil
	}
	// Lazy init: one router per Server instance.
	// No need for sync.Once; concurrent initialization is benign.
	if s.grpcServer == nil {
		// Start() sets grpcServer before interceptors are used; keep defensive.
	}
	if s.apiRoute == nil {
		s.apiRoute = &apiRouter{s: s}
	}
	return s.apiRoute
}

// APIRouteGRPCPeers returns the peer gRPC endpoints used by APIRoute.
// Priority: global.apiRoute.nodes > global.apiRoute.grpcPeers > cluster.sync (address + seedservers).
func (s *Server) APIRouteGRPCPeers() []string {
	if s == nil {
		return nil
	}
	return s.apiRouteGRPCPeers(s.GetCommonConf().APIRoute)
}

func (s *Server) apiRouteGRPCPeers(conf cfg.APIRoute) []string {
	peers := apiRouteGRPCPeersFromConf(conf)
	if len(peers) == 0 {
		peers = s.apiRouteGRPCPeersFromRawClusterSync()
	}
	if len(peers) == 0 {
		return nil
	}
	return apiRouteFilterAndDedupePeers(s.GetCommonConf().TCP.ListenAddr, peers)
}

func apiRouteGRPCPeersFromConf(conf cfg.APIRoute) []string {
	if len(conf.Nodes) > 0 {
		out := make([]string, 0, len(conf.Nodes))
		for _, n := range conf.Nodes {
			if n.GRPC != "" {
				out = append(out, n.GRPC)
			}
		}
		return out
	}
	return conf.GRPCPeers
}

func apiRouteFilterAndDedupePeers(localListenAddr string, peers []string) []string {
	if len(peers) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(peers))
	out := make([]string, 0, len(peers))
	for _, p := range peers {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if apiRouteIsSelfPeer(localListenAddr, p) {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

func apiRouteIsSelfPeer(localListenAddr, peer string) bool {
	lp := normalizePort(localListenAddr)
	pp := normalizePort(peer)
	if lp == "" || pp == "" || lp != pp {
		return false
	}
	// When we listen on ":port", treat "localhost/127.0.0.1/::1:port" as self.
	host, _, err := net.SplitHostPort(peer)
	if err != nil {
		return true
	}
	host = strings.Trim(host, "[]")
	switch host {
	case "", "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

func (s *Server) apiRouteGRPCPeersFromRawClusterSync() []string {
	// Support the legacy/example config style:
	//
	// cluster:
	//   sync:
	//     address: "localhost:50052"
	//     seedservers: ["localhost:50053", "localhost:50054"]
	//
	// This lets users configure the cluster once and let apiRoute reuse the peer list.
	if s == nil || s.rawConfig == nil {
		return nil
	}
	cluster := s.rawConfig["cluster"]
	if cluster == nil {
		return nil
	}
	syncConfAny, ok := cluster["sync"]
	if !ok || syncConfAny == nil {
		return nil
	}
	syncConf, ok := syncConfAny.(map[string]any)
	if !ok {
		return nil
	}
	var peers []string
	if addr, _ := syncConf["address"].(string); strings.TrimSpace(addr) != "" {
		peers = append(peers, strings.TrimSpace(addr))
	}
	seedsAny, ok := syncConf["seedservers"]
	if !ok {
		// allow camelCase
		seedsAny = syncConf["seedServers"]
	}
	for _, s2 := range apiRouteCoerceStringSlice(seedsAny) {
		if s2 = strings.TrimSpace(s2); s2 != "" {
			peers = append(peers, s2)
		}
	}
	return peers
}

func apiRouteCoerceStringSlice(v any) []string {
	switch vv := v.(type) {
	case nil:
		return nil
	case []string:
		return vv
	case []any:
		out := make([]string, 0, len(vv))
		for _, x := range vv {
			switch s := x.(type) {
			case string:
				out = append(out, s)
			default:
				// ignore
			}
		}
		return out
	case string:
		// Accept "a,b,c" for convenience.
		if strings.Contains(vv, ",") {
			parts := strings.Split(vv, ",")
			out := make([]string, 0, len(parts))
			for _, p := range parts {
				if p = strings.TrimSpace(p); p != "" {
					out = append(out, p)
				}
			}
			return out
		}
		if strings.TrimSpace(vv) != "" {
			return []string{vv}
		}
		return nil
	default:
		// YAML may decode numeric ports; accept that best-effort.
		switch n := v.(type) {
		case int:
			return []string{strconv.Itoa(n)}
		case int64:
			return []string{strconv.FormatInt(n, 10)}
		case float64:
			return []string{strconv.FormatInt(int64(n), 10)}
		}
		return nil
	}
}

// RouteInterceptor routes configured methods to the node hosting the stream.
func (s *Server) RouteInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		conf := s.GetCommonConf().APIRoute
		if !conf.Enable {
			return handler(ctx, req)
		}
		peers := s.apiRouteGRPCPeers(conf)
		if len(peers) == 0 {
			return handler(ctx, req)
		}

		// Prevent forwarding loops.
		if md, ok := metadata.FromIncomingContext(ctx); ok && len(md.Get(apiRouteForwardedMetaKey)) > 0 {
			return handler(ctx, req)
		}

		// Route only methods that are registered (or explicitly configured).
		if len(conf.Methods) > 0 {
			routeThis := false
			for _, m := range conf.Methods {
				if m == info.FullMethod {
					routeThis = true
					break
				}
			}
			if !routeThis {
				return handler(ctx, req)
			}
		} else {
			if _, ok := apiRouteUnaryFactory(info.FullMethod); !ok {
				return handler(ctx, req)
			}
		}

		streamPath, ok := extractStreamPath(req)
		if !ok || streamPath == "" {
			return handler(ctx, req)
		}

		// If stream is local, execute locally.
		if _, exists := s.Streams.SafeGet(streamPath); exists {
			return handler(ctx, req)
		}

		router := s.apiRouter()
		if router == nil {
			return handler(ctx, req)
		}

		now := time.Now()
		if cachedTarget, ok := router.cacheGet(streamPath, now); ok && cachedTarget != "" && !apiRouteIsSelfPeer(s.GetCommonConf().TCP.ListenAddr, cachedTarget) {
			return router.forwardUnary(ctx, info.FullMethod, req, cachedTarget, conf.ForwardTimeout)
		}

		target, err := router.resolveStreamTarget(ctx, streamPath, peers, conf.ResolveTimeout)
		if err != nil || target == "" {
			return nil, status.Errorf(codes.NotFound, "stream not found in cluster: %s", streamPath)
		}
		if apiRouteIsSelfPeer(s.GetCommonConf().TCP.ListenAddr, target) {
			return handler(ctx, req)
		}
		router.cacheSet(streamPath, target, conf.CacheTTL, now)
		return router.forwardUnary(ctx, info.FullMethod, req, target, conf.ForwardTimeout)
	}
}

func (r *apiRouter) resolveStreamTarget(ctx context.Context, streamPath string, peers []string, timeout time.Duration) (string, error) {
	if streamPath == "" || len(peers) == 0 {
		return "", status.Error(codes.NotFound, "no peers")
	}
	localListenAddr := r.s.GetCommonConf().TCP.ListenAddr
	for _, peer := range peers {
		if peer == "" || apiRouteIsSelfPeer(localListenAddr, peer) {
			continue
		}
		checkCtx := ctx
		var cancel context.CancelFunc = func() {}
		if timeout > 0 {
			checkCtx, cancel = context.WithTimeout(ctx, timeout)
		}

		conn, err := r.getConn(checkCtx, peer)
		if err != nil {
			cancel()
			continue
		}
		client := pb.NewApiClient(conn)
		_, err = client.StreamInfo(checkCtx, &pb.StreamSnapRequest{StreamPath: streamPath})
		cancel()
		if err == nil {
			return peer, nil
		}
	}
	return "", status.Error(codes.NotFound, "stream not found")
}

func (r *apiRouter) forwardUnary(ctx context.Context, fullMethod string, req any, target string, timeout time.Duration) (any, error) {
	if fullMethod == "" || target == "" {
		return nil, status.Error(codes.Internal, "invalid route")
	}

	factory, ok := apiRouteUnaryFactory(fullMethod)
	if !ok || factory == nil {
		return nil, status.Errorf(codes.Unimplemented, "route response not registered for method: %s", fullMethod)
	}
	resp := factory()
	if resp == nil {
		return nil, status.Errorf(codes.Unimplemented, "route response factory returned nil for method: %s", fullMethod)
	}

	// Copy incoming metadata to outgoing, and mark forwarded.
	var inMD metadata.MD
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		inMD = md.Copy()
	} else {
		inMD = metadata.MD{}
	}
	inMD.Set(apiRouteForwardedMetaKey, "1")
	outCtx := metadata.NewOutgoingContext(ctx, inMD)

	if timeout > 0 {
		var cancel context.CancelFunc
		outCtx, cancel = context.WithTimeout(outCtx, timeout)
		defer cancel()
	}

	conn, err := r.getConn(outCtx, target)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "dial target %s failed: %v", target, err)
	}
	if err := conn.Invoke(outCtx, fullMethod, req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}
