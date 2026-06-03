//go:build cluster

package plugin_cluster

import (
	"net/url"
	"strings"
)

// GetRedirectTarget 实现 m7s.RedirectAdvisor。委托给 V2 + 空 scheme。
func (p *ClusterPlugin) GetRedirectTarget(protocol, streamPath, currentHost string) (string, int, bool) {
	return p.GetRedirectTargetV2(protocol, streamPath, currentHost, "")
}

// GetRedirectTargetV2 实现 m7s.RedirectAdvisorV2。把流路径 → owner peer 的 advertise 端口。
//
// 行为(§4 路线 B):
//   - cluster 未就绪 / 流不在任何节点 → ok=false,m7s 走默认逻辑
//   - 流在本节点 → ok=false,m7s 本地处理
//   - 流在远端 → 返回 peer 对应 protocol 的 advertise host:port + 302
func (p *ClusterPlugin) GetRedirectTargetV2(protocol, streamPath, _ /*currentHost*/, _ /*scheme*/ string) (targetHost string, statusCode int, ok bool) {
	if p.streamRegistry == nil || p.membership == nil {
		return "", 0, false
	}
	owner, found := p.streamRegistry.Lookup(streamPath)
	if !found {
		return "", 0, false
	}
	if owner == p.NodeID {
		return "", 0, false
	}
	peer, found := p.membership.Peer(owner)
	if !found {
		p.Warn("redirect: owner peer not in membership table", "streamPath", streamPath, "owner", owner)
		return "", 0, false
	}

	var raw string
	switch strings.ToLower(protocol) {
	case "rtsp":
		raw = peer.Advertise.RTSP
	default:
		raw = peer.Advertise.FLV
	}
	if raw == "" {
		p.Warn("redirect: peer has no advertise for protocol", "streamPath", streamPath, "owner", owner, "protocol", protocol)
		return "", 0, false
	}
	// FLV advertise 形如 http://host:port,需要剥 scheme 只留 host:port(m7s 给的 targetHost 没有 scheme)。
	// RTSP advertise 已是 host:port,strip 无副作用。
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host, 302, true
	}
	return raw, 302, true
}
