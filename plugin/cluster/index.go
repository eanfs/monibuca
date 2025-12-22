package plugin_claster

import (
	"strings"
	"time"

	m7s "m7s.live/v5"
)

// Pragmatic cluster plugin:
// - consumes static peer config under `cluster:` (sync/seedservers or peers)
// - optionally auto-enables global apiRoute and populates grpcPeers for APIRoute usage
var _ = m7s.InstallPlugin[ClusterPlugin](m7s.PluginMeta{})

type ClusterPlugin struct {
	m7s.Plugin

	// Peers is an optional static peer list (grpc host:port).
	// When set, it will be used in addition to Sync.Address/SeedServers.
	Peers []string `desc:"静态 peer 列表（grpc host:port），可替代或补充 sync.address/seedservers。"`

	Sync ClusterSync `desc:"静态邻居表配置，用于小规模集群（无 etcd/服务发现）。"`

	AutoEnableAPIRoute bool `default:"true" desc:"当配置了 cluster 时自动启用 global.apiRoute.enable（可在 global.apiRoute.enable=false 显式关闭）。"`
}

type ClusterSync struct {
	ServerID           string        `desc:"节点 ID（仅用于标识/日志）"`
	Address            string        `desc:"本节点 gRPC 地址（host:port），可被 peers 引用"`
	SeedServers        []string      `desc:"静态种子节点列表（grpc host:port）"`
	HeartbeatInterval  time.Duration `default:"5s" desc:"心跳间隔（当前主要用于文档/保留字段）"`
	SyncInterval       time.Duration `default:"30s" desc:"同步间隔（当前主要用于文档/保留字段）"`
	ResolveTimeoutHint time.Duration `default:"800ms" desc:"探测超时建议值（可映射到 global.apiRoute.resolveTimeout）"`
}

func (p *ClusterPlugin) Start() (err error) {
	if !p.isConfigured() {
		return nil
	}
	common := p.GetGlobalCommonConf()

	if p.AutoEnableAPIRoute && !common.APIRoute.Enable {
		common.APIRoute.Enable = true
	}

	// Populate grpcPeers for APIRoute if user didn't explicitly configure it.
	if len(common.APIRoute.Nodes) == 0 && len(common.APIRoute.GRPCPeers) == 0 {
		common.APIRoute.GRPCPeers = p.grpcPeers()
	}

	// Best-effort: provide a sane resolve timeout hint if unset.
	if common.APIRoute.ResolveTimeout == 0 && p.Sync.ResolveTimeoutHint > 0 {
		common.APIRoute.ResolveTimeout = p.Sync.ResolveTimeoutHint
	}

	return nil
}

func (p *ClusterPlugin) isConfigured() bool {
	if len(p.Peers) > 0 {
		return true
	}
	if p.Sync.Address != "" {
		return true
	}
	return len(p.Sync.SeedServers) > 0
}

func (p *ClusterPlugin) grpcPeers() []string {
	out := make([]string, 0, 1+len(p.Peers)+len(p.Sync.SeedServers))
	if p.Sync.Address != "" {
		out = append(out, strings.TrimSpace(p.Sync.Address))
	}
	for _, s := range p.Sync.SeedServers {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	for _, s := range p.Peers {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	seen := make(map[string]struct{}, len(out))
	dedup := out[:0]
	for _, s := range out {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		dedup = append(dedup, s)
	}
	return dedup
}
