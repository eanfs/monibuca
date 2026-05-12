//go:build cluster

package plugin_cluster

import (
	"net/url"
	"time"

	task "github.com/langhuihui/gotask"
	cfg "m7s.live/v5/pkg/config"
)

// buildAPIRouteNodes 把 Membership.Peers() 翻译成 m7s.APIRouteNode 列表,排除 self。
// 一个 peer 必须至少有 GRPC / FLV / RTSP 三者之一才被纳入(避免给 RouteInterceptor
// 推空地址)。FLV advertise 已带 scheme,写入 APIRouteNode.HTTP 时用 url.Parse 取 Host。
func buildAPIRouteNodes(peers []*PeerInfo, selfNodeID string) []cfg.APIRouteNode {
	out := make([]cfg.APIRouteNode, 0, len(peers))
	for _, p := range peers {
		if p == nil || p.NodeID == selfNodeID {
			continue
		}
		var http string
		if p.Advertise.FLV != "" {
			if u, err := url.Parse(p.Advertise.FLV); err == nil && u.Host != "" {
				http = u.Host
			} else {
				http = p.Advertise.FLV
			}
		}
		if p.Advertise.GRPC == "" && http == "" && p.Advertise.RTSP == "" {
			continue
		}
		out = append(out, cfg.APIRouteNode{
			GRPC: p.Advertise.GRPC,
			HTTP: http,
			RTSP: p.Advertise.RTSP,
		})
	}
	return out
}

// peerSyncTask 每 2s 把 Membership.Peers() 同步成 APIRoute.Nodes 写入 m7s 全局 config,
// 同时把 APIRoute.Enable 设为 true。给现有 RouteInterceptor 提供 cluster 感知的 peer 列表。
type peerSyncTask struct {
	task.TickTask
	plugin *ClusterPlugin
}

func (t *peerSyncTask) GetTickInterval() time.Duration {
	return 2 * time.Second
}

func (t *peerSyncTask) Tick(_ any) {
	if t.plugin == nil || t.plugin.Server == nil || t.plugin.membership == nil {
		return
	}
	nodes := buildAPIRouteNodes(t.plugin.membership.Peers(), t.plugin.NodeID)
	conf := t.plugin.Server.GetCommonConf()
	conf.APIRoute.Nodes = nodes
	conf.APIRoute.Enable = true
}
