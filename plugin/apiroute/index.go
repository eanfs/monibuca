package plugin_apiroute

import (
	"context"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
	m7s "m7s.live/v5"
	"m7s.live/v5/pb"
	cfg "m7s.live/v5/pkg/config"
)

var _ = m7s.InstallPlugin[APIRoutePlugin](m7s.PluginMeta{})

type routeCacheEntry struct {
	targetGRPC string
	expAt      time.Time
}

type peerPorts struct {
	expAt time.Time
	// protocol -> scheme -> port
	ports map[string]map[string]string
}

type APIRoutePlugin struct {
	m7s.Plugin
	mu    sync.Mutex
	cache map[string]routeCacheEntry
	conns sync.Map // map[string]*grpc.ClientConn

	peerMu    sync.Mutex
	peerCache map[string]peerPorts // key: grpc addr
}

func (p *APIRoutePlugin) Start() (err error) {
	conf := p.GetGlobalCommonConf().APIRoute
	if !conf.Enable {
		return nil
	}
	nodes := apiRoutePeers(p, conf)
	// Warm up peer port discovery in background to keep redirects fast.
	go func() {
		for _, n := range nodes {
			if n.GRPC == "" || p.isSameGRPCEndpoint(n.GRPC) {
				continue
			}
			ctx, cancel := context.WithTimeout(p.Context, time.Second*3)
			_, _ = p.discoverPeerPorts(ctx, n.GRPC, time.Minute*5)
			cancel()
		}
	}()
	return nil
}

func normalizePort(addr string) string {
	if addr == "" {
		return ""
	}
	if strings.HasPrefix(addr, ":") {
		return strings.TrimPrefix(addr, ":")
	}
	_, port, err := net.SplitHostPort(addr)
	if err == nil && port != "" {
		return port
	}
	if i := strings.LastIndex(addr, ":"); i >= 0 && i < len(addr)-1 {
		return addr[i+1:]
	}
	return ""
}

func (p *APIRoutePlugin) isSameGRPCEndpoint(peer string) bool {
	lp := normalizePort(p.GetGlobalCommonConf().TCP.ListenAddr)
	pp := normalizePort(peer)
	if lp == "" || pp == "" || lp != pp {
		return false
	}
	// When listening on ":port", treat loopback peers as self; do not treat other hosts as self.
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

func (p *APIRoutePlugin) getConn(ctx context.Context, target string) (*grpc.ClientConn, error) {
	if v, ok := p.conns.Load(target); ok {
		return v.(*grpc.ClientConn), nil
	}
	conn, err := grpc.DialContext(ctx, target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	actual, loaded := p.conns.LoadOrStore(target, conn)
	if loaded {
		_ = conn.Close()
		return actual.(*grpc.ClientConn), nil
	}
	p.Using(conn)
	return conn, nil
}

func (p *APIRoutePlugin) cacheGet(streamPath string, now time.Time) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cache == nil {
		p.cache = make(map[string]routeCacheEntry)
		return "", false
	}
	ent, ok := p.cache[streamPath]
	if !ok || ent.targetGRPC == "" {
		return "", false
	}
	if !ent.expAt.IsZero() && now.After(ent.expAt) {
		delete(p.cache, streamPath)
		return "", false
	}
	return ent.targetGRPC, true
}

func (p *APIRoutePlugin) cacheSet(streamPath, target string, ttl time.Duration, now time.Time) {
	if ttl <= 0 || streamPath == "" || target == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cache == nil {
		p.cache = make(map[string]routeCacheEntry)
	}
	p.cache[streamPath] = routeCacheEntry{targetGRPC: target, expAt: now.Add(ttl)}
}

func apiRoutePeers(p *APIRoutePlugin, conf cfg.APIRoute) []cfg.APIRouteNode {
	if len(conf.Nodes) > 0 {
		return conf.Nodes
	}
	peers := conf.GRPCPeers
	if len(peers) == 0 && p != nil && p.Server != nil {
		peers = p.Server.APIRouteGRPCPeers()
	}
	out := make([]cfg.APIRouteNode, 0, len(peers))
	for _, g := range peers {
		if g != "" {
			out = append(out, cfg.APIRouteNode{GRPC: g})
		}
	}
	return out
}

func protocolKeyFromPluginName(name string) string {
	switch strings.ToLower(name) {
	case "mp4":
		return "mp4"
	case "flv":
		return "flv"
	case "hls":
		return "hls"
	case "llhls":
		return "hls"
	case "webrtc":
		return "webrtc"
	case "rtsp":
		return "rtsp"
	default:
		return ""
	}
}

func ensurePortMap(m map[string]map[string]string, proto, scheme, port string) {
	if proto == "" || scheme == "" || port == "" {
		return
	}
	if _, ok := m[proto]; !ok {
		m[proto] = make(map[string]string)
	}
	if _, ok := m[proto][scheme]; !ok {
		m[proto][scheme] = port
	}
}

func parsePortFromPlayAddr(playAddr string) (scheme, port string, ok bool) {
	if playAddr == "" {
		return "", "", false
	}
	// Replace placeholder to make it parseable.
	playAddr = strings.ReplaceAll(playAddr, "{hostName}", "example.com")
	u, err := url.Parse(playAddr)
	if err != nil || u.Scheme == "" {
		return "", "", false
	}
	scheme = strings.ToLower(u.Scheme)
	_, p2, err := net.SplitHostPort(u.Host)
	if err == nil && p2 != "" {
		return scheme, p2, true
	}
	// Default ports
	switch scheme {
	case "http":
		return scheme, "80", true
	case "https":
		return scheme, "443", true
	case "rtsp":
		return scheme, "554", true
	}
	return "", "", false
}

func (p *APIRoutePlugin) peerPortsGet(grpcAddr string, now time.Time) (peerPorts, bool) {
	p.peerMu.Lock()
	defer p.peerMu.Unlock()
	if p.peerCache == nil {
		p.peerCache = make(map[string]peerPorts)
		return peerPorts{}, false
	}
	ent, ok := p.peerCache[grpcAddr]
	if !ok {
		return peerPorts{}, false
	}
	if !ent.expAt.IsZero() && now.After(ent.expAt) {
		delete(p.peerCache, grpcAddr)
		return peerPorts{}, false
	}
	return ent, true
}

func (p *APIRoutePlugin) peerPortsSet(grpcAddr string, ports peerPorts) {
	p.peerMu.Lock()
	defer p.peerMu.Unlock()
	if p.peerCache == nil {
		p.peerCache = make(map[string]peerPorts)
	}
	p.peerCache[grpcAddr] = ports
}

func (p *APIRoutePlugin) discoverPeerPorts(ctx context.Context, grpcAddr string, ttl time.Duration) (peerPorts, bool) {
	now := time.Now()
	if ent, ok := p.peerPortsGet(grpcAddr, now); ok {
		return ent, true
	}
	conn, err := p.getConn(ctx, grpcAddr)
	if err != nil {
		return peerPorts{}, false
	}
	client := pb.NewApiClient(conn)
	resp, err := client.SysInfo(ctx, &emptypb.Empty{})
	if err != nil || resp == nil || resp.Data == nil {
		return peerPorts{}, false
	}
	m := make(map[string]map[string]string)
	for _, pi := range resp.Data.Plugins {
		if pi == nil {
			continue
		}
		protoKey := protocolKeyFromPluginName(pi.Name)
		if protoKey == "" {
			continue
		}
		for _, play := range pi.PlayAddr {
			scheme, port, ok := parsePortFromPlayAddr(play)
			if !ok {
				continue
			}
			ensurePortMap(m, protoKey, scheme, port)
		}
	}
	ent := peerPorts{ports: m}
	if ttl <= 0 {
		ttl = time.Minute * 5
	}
	ent.expAt = now.Add(ttl)
	p.peerPortsSet(grpcAddr, ent)
	return ent, true
}

func (p *APIRoutePlugin) resolveStreamTargetGRPC(ctx context.Context, streamPath string, nodes []cfg.APIRouteNode, timeout time.Duration) (string, bool) {
	if streamPath == "" || len(nodes) == 0 {
		return "", false
	}
	now := time.Now()
	if cached, ok := p.cacheGet(streamPath, now); ok && cached != "" {
		return cached, true
	}
	for _, n := range nodes {
		if n.GRPC == "" || p.isSameGRPCEndpoint(n.GRPC) {
			continue
		}
		checkCtx := ctx
		var cancel context.CancelFunc = func() {}
		if timeout > 0 {
			checkCtx, cancel = context.WithTimeout(ctx, timeout)
		}
		conn, err := p.getConn(checkCtx, n.GRPC)
		if err != nil {
			cancel()
			continue
		}
		client := pb.NewApiClient(conn)
		_, err = client.StreamInfo(checkCtx, &pb.StreamSnapRequest{StreamPath: streamPath})
		cancel()
		if err == nil {
			p.cacheSet(streamPath, n.GRPC, p.GetGlobalCommonConf().APIRoute.CacheTTL, now)
			return n.GRPC, true
		}
	}
	return "", false
}

func (p *APIRoutePlugin) mapTarget(protocol, scheme, targetGRPC string, nodes []cfg.APIRouteNode) (string, bool) {
	// Prefer explicit config if provided.
	for _, n := range nodes {
		if n.GRPC != targetGRPC {
			continue
		}
		switch protocol {
		case "rtsp":
			if n.RTSP != "" {
				return n.RTSP, true
			}
		default:
			if n.HTTP != "" {
				return n.HTTP, true
			}
		}
		// Matched node but no explicit target for this protocol; fall back to discovery.
		break
	}
	// Auto-discover ports from SysInfo and combine with grpc host.
	host, _, err := net.SplitHostPort(targetGRPC)
	if err != nil || host == "" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(p.Context, time.Second*2)
	defer cancel()
	ports, ok := p.discoverPeerPorts(ctx, targetGRPC, time.Minute*5)
	if !ok || ports.ports == nil {
		return "", false
	}
	proto := protocol
	if proto == "" {
		return "", false
	}
	if proto == "rtsp" {
		if port, ok := ports.ports["rtsp"]["rtsp"]; ok && port != "" {
			return net.JoinHostPort(host, port), true
		}
		return "", false
	}
	if scheme == "" {
		scheme = "http"
	}
	if port, ok := ports.ports[proto][scheme]; ok && port != "" {
		return net.JoinHostPort(host, port), true
	}
	// Fallback: use http port if scheme-specific not found.
	if port, ok := ports.ports[proto]["http"]; ok && port != "" {
		return net.JoinHostPort(host, port), true
	}
	return "", false
}

func stripLeadingProtocol(protocol, raw string) string {
	if raw == "" || protocol == "" {
		return strings.TrimPrefix(raw, "/")
	}
	raw = strings.TrimPrefix(raw, "/")
	protoPrefix := protocol + "/"
	if strings.HasPrefix(raw, protoPrefix) {
		raw = strings.TrimPrefix(raw, protoPrefix)
	}
	return raw
}

func stripKnownExt(raw string) string {
	switch {
	case strings.HasSuffix(raw, ".mp4"):
		return strings.TrimSuffix(raw, ".mp4")
	case strings.HasSuffix(raw, ".fmp4"):
		return strings.TrimSuffix(raw, ".fmp4")
	case strings.HasSuffix(raw, ".flv"):
		return strings.TrimSuffix(raw, ".flv")
	case strings.HasSuffix(raw, ".m3u8"):
		return strings.TrimSuffix(raw, ".m3u8")
	case strings.HasSuffix(raw, ".ts"):
		return strings.TrimSuffix(raw, ".ts")
	default:
		return raw
	}
}

func candidateStreamPaths(protocol, raw string) []string {
	// Many built-in plugins pass redirectPath including the plugin prefix (e.g. "mp4/live/a.mp4").
	// HLS and others may request segment paths (e.g. "hls/live/a/index.m3u8" or "hls/live/a/xxx.ts").
	normalized := stripKnownExt(stripLeadingProtocol(protocol, raw))
	normalized = strings.TrimSuffix(normalized, "/index")
	normalized = strings.TrimSuffix(normalized, "/playlist")

	if normalized == "" {
		return nil
	}
	out := []string{normalized}
	// Try stripping trailing segments to map playlist/segment requests back to the publisher streamPath.
	for i := 0; i < 6; i++ {
		j := strings.LastIndex(normalized, "/")
		if j <= 0 {
			break
		}
		normalized = normalized[:j]
		out = append(out, normalized)
	}
	return out
}

// GetRedirectTarget implements m7s.RedirectAdvisor.
// It redirects playback requests to the node that actually hosts the stream.
func (p *APIRoutePlugin) GetRedirectTarget(protocol, streamPath, currentHost string) (targetHost string, statusCode int, ok bool) {
	return p.GetRedirectTargetV2(protocol, streamPath, currentHost, "http")
}

// GetRedirectTargetV2 implements m7s.RedirectAdvisorV2.
func (p *APIRoutePlugin) GetRedirectTargetV2(protocol, streamPath, currentHost, scheme string) (targetHost string, statusCode int, ok bool) {
	conf := p.GetGlobalCommonConf().APIRoute
	if !conf.Enable {
		return "", 0, false
	}
	nodes := apiRoutePeers(p, conf)
	var targetGRPC string
	for _, candidate := range candidateStreamPaths(protocol, streamPath) {
		if candidate == "" {
			continue
		}
		if grpcTarget, found := p.resolveStreamTargetGRPC(p.Context, candidate, nodes, conf.ResolveTimeout); found && grpcTarget != "" {
			targetGRPC = grpcTarget
			break
		}
	}
	if targetGRPC == "" {
		return "", 0, false
	}
	targetHost, ok = p.mapTarget(protocol, strings.ToLower(scheme), targetGRPC, nodes)
	if !ok || targetHost == "" {
		return "", 0, false
	}
	if targetHost == currentHost {
		return "", 0, false
	}
	// Use Found for broad client compatibility.
	return targetHost, 302, true
}
