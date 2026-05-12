//go:build cluster

package plugin_cluster

import (
	"errors"

	m7s "m7s.live/v5"
)

// ClusterPlugin 是 cluster 插件主体。
//
// Phase 1 范围:Membership 模块 + /api/cluster/nodes。
// Phase 2 范围:StreamRegistry 模块 + /api/cluster/streams。
// 后续阶段会逐步附上 Relay / StreamLocator / LoadReporter。
type ClusterPlugin struct {
	m7s.Plugin
	NodeID         string          `desc:"节点 ID,全局唯一,必填"`
	Consul         ConsulConfig    `desc:"Consul 服务发现配置"`
	Advertise      AdvertiseConfig `desc:"对外宣告的协议端口表,跨节点拉流时对端会用"`
	RelayProtocols []string        `default:"rtmp,rtsp,flv" desc:"跨节点拉流协议优先级(Phase 3 用)"`
	LoadShed       LoadShedConfig  `desc:"负载卸载策略(Phase 6 用)"`

	membership     *Membership
	streamRegistry *StreamRegistry
	// relayHook 是 Phase 3 Relay 创建 cluster-relay pull-proxy 的注入点。
	// 默认 nil → ensureRelay 走 p.Server.EnsurePullProxy。测试可 swap。
	relayHook RelayHook
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
	if err := p.AddTask(p.membership).WaitStarted(); err != nil {
		return err
	}
	p.streamRegistry = newStreamRegistry(p)
	if err := p.AddTask(p.streamRegistry).WaitStarted(); err != nil {
		return err
	}
	return nil
}

// OnPublish 实现 m7s.IPublishHookPlugin。本地有新 publisher 时,
// 通知 StreamRegistry 把流位置写到 Consul(cluster-relay 派生的 publisher 会被跳过)。
func (p *ClusterPlugin) OnPublish(pub *m7s.Publisher) {
	if p.streamRegistry != nil {
		p.streamRegistry.OnPublish(pub)
	}
}

// Membership 暴露给同包内其它模块(Phase 3/4)读取 peers 与 sessionID。
func (p *ClusterPlugin) Membership() *Membership { return p.membership }

// StreamRegistry 暴露给同包内其它模块(Phase 3/4)读取流位置。
func (p *ClusterPlugin) StreamRegistry() *StreamRegistry { return p.streamRegistry }
