//go:build cluster

package plugin_cluster

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	m7s "m7s.live/v5"
)

// buildPullURL 根据 RelayProtocols 优先级 + peer 的 Advertise 表,选第一个
// peer 真实在监听的协议,拼出 m7s Puller 可直接吃下的完整 URL。
//
// 协议惯例:
//   - rtmp: addr 是 host:port,完整 URL = rtmp://addr/streamPath
//   - rtsp: 同上,URL = rtsp://addr/streamPath
//   - flv : 配置里已经带 scheme(http:// 或 https://),URL = <flvAddr>/streamPath.flv
//
// 全部协议都没匹配上时返回 error,调用方记 Warn 后放弃 relay。
func buildPullURL(peer *PeerInfo, streamPath string, priority []string) (proto, fullURL string, err error) {
	for _, p := range priority {
		switch p {
		case "rtmp":
			if addr := peer.Advertise.RTMP; addr != "" {
				return "rtmp", "rtmp://" + addr + "/" + streamPath, nil
			}
		case "rtsp":
			if addr := peer.Advertise.RTSP; addr != "" {
				return "rtsp", "rtsp://" + addr + "/" + streamPath, nil
			}
		case "flv":
			if addr := peer.Advertise.FLV; addr != "" {
				return "flv", addr + "/" + streamPath + ".flv", nil
			}
		}
	}
	return "", "", fmt.Errorf("no advertised protocol matches priority %v for peer %s", priority, peer.NodeID)
}

// RelayHook 是 ensureRelay 实际执行 pull-proxy 创建的注入点。生产实现走
// Server.EnsurePullProxy,测试可换成 recorder fake。
//
// 用 any 而不是 *m7s.PullProxyConfig 是 Phase 3 中间阶段的简化 —— 主插件
// 编织 conf 后直接传 *m7s.PullProxyConfig,helper 只 record。Task 10 时会
// 收窄类型(改成 *m7s.PullProxyConfig)。
type RelayHook = func(conf any) (created bool, err error)

// OnSubscribe 实现 m7s.ISubscribeHookPlugin。本节点出现订阅但没有对应本地流时:
//  1. StreamRegistry.Lookup 找 origin 节点 id
//  2. 若找不到,return(订阅者会在等待队列里超时)
//  3. 若 owner == self,return(本地流,m7s 主流程自然处理)
//  4. 否则 Membership.Peer 找 origin 的 advertise 表,buildPullURL 拼 URL
//  5. relayHook(conf) 起一个 cluster-relay pull-proxy
//
// 任何步骤失败都 log Warn 后 return,不影响订阅者本身(超时由 m7s 处理)。
func (p *ClusterPlugin) OnSubscribe(streamPath string, _ url.Values) {
	if p.streamRegistry == nil || p.membership == nil {
		return
	}
	originID, ok := p.streamRegistry.Lookup(streamPath)
	if !ok {
		return
	}
	if originID == p.NodeID {
		return
	}
	peer, ok := p.membership.Peer(originID)
	if !ok {
		p.Warn("relay: origin peer not in membership table", "streamPath", streamPath, "originId", originID)
		return
	}
	proto, fullURL, err := buildPullURL(peer, streamPath, p.RelayProtocols)
	if err != nil {
		p.Warn("relay: no matching protocol", "streamPath", streamPath, "originId", originID, "error", err)
		return
	}
	if err := p.ensureRelay(originID, streamPath, proto, fullURL); err != nil {
		p.Warn("relay: ensure pull proxy failed", "streamPath", streamPath, "error", err)
	}
}

// ensureRelay 把 relay 参数组装成 *m7s.PullProxyConfig 并调用注入点(默认走 Server.EnsurePullProxy)。
// Description 字段注入 cluster-relay 标记,防止 C→B→A→B 环回(§3.4)。
// 成功后把 streamPath 写入 activeRelays,供 origin 失联时快速查找。
func (p *ClusterPlugin) ensureRelay(originID, streamPath, proto, fullURL string) error {
	conf := &m7s.PullProxyConfig{
		StreamPath:  streamPath,
		Type:        proto,
		Description: ClusterRelayDescPrefix + originID,
		PullOnStart: false,
		StopOnIdle:  true,
	}
	conf.URL = fullURL
	conf.MaxRetry = 3
	conf.RetryInterval = time.Second

	// 确保 onStreamRemoved 钩子已注册(生产路径由 Start() 负责;测试路径
	// 绕开 Start(),由此处懒初始化补齐)。
	p.setupRelayHooks()

	var err error
	if hook := p.relayHook; hook != nil {
		_, err = hook(conf)
	} else if p.Server != nil {
		_, _, err = p.Server.EnsurePullProxy(conf)
	} else {
		err = fmt.Errorf("server not attached")
	}
	if err == nil {
		p.activeRelaysMu.Lock()
		if p.activeRelays == nil {
			p.activeRelays = make(map[string]struct{})
		}
		p.activeRelays[streamPath] = struct{}{}
		p.activeRelaysMu.Unlock()
	}
	return err
}

// stopRelayPullProxy 生产路径:遍历 Server 的 pull-proxies,找 Description 带
// ClusterRelayDescPrefix 前缀且 StreamPath 匹配的那一个,Stop(reason)。
//
// 用 Description 前缀 + StreamPath 双重确认,避免误杀用户自配的同名 pull-proxy。
func (p *ClusterPlugin) stopRelayPullProxy(streamPath string, reason error) {
	if p.Server == nil {
		return
	}
	proxy, ok := p.Server.PullProxies.Find(func(proxy m7s.IPullProxy) bool {
		conf := proxy.GetConfig()
		if conf == nil {
			return false
		}
		return conf.StreamPath == streamPath &&
			strings.HasPrefix(conf.Description, ClusterRelayDescPrefix)
	})
	if ok {
		proxy.Stop(reason)
	} else {
		p.Warn("relay: no active cluster-relay pull proxy found to stop", "streamPath", streamPath)
	}
}

var _ m7s.ISubscribeHookPlugin = (*ClusterPlugin)(nil)
