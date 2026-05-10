//go:build cluster

package plugin_cluster

import (
	"errors"

	m7s "m7s.live/v5"
)

// ClusterPlugin 是 cluster 插件主体。
//
// Phase 1 范围:Membership 模块 + /api/cluster/nodes。
// 后续阶段会逐步附上 StreamRegistry / Relay / StreamLocator / LoadReporter。
type ClusterPlugin struct {
	m7s.Plugin
	NodeID         string          `desc:"节点 ID,全局唯一,必填"`
	Consul         ConsulConfig    `desc:"Consul 服务发现配置"`
	Advertise      AdvertiseConfig `desc:"对外宣告的协议端口表,跨节点拉流时对端会用"`
	RelayProtocols []string        `default:"rtmp,rtsp,flv" desc:"跨节点拉流协议优先级(Phase 3 用)"`
	LoadShed       LoadShedConfig  `desc:"负载卸载策略(Phase 6 用)"`

	membership *Membership
}

var _ = m7s.InstallPlugin[ClusterPlugin](m7s.PluginMeta{})

func (p *ClusterPlugin) Start() error {
	if p.NodeID == "" {
		return errors.New("cluster.nodeid is required")
	}
	if len(p.Consul.Addresses) == 0 {
		return errors.New("cluster.consul.addresses must contain at least one entry")
	}
	p.membership = newMembership(p)
	return p.AddTask(p.membership).WaitStarted()
}

// Membership 暴露给同包内其它模块(Phase 2/3/4)读取 peers 与 sessionID。
func (p *ClusterPlugin) Membership() *Membership { return p.membership }
