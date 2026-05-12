//go:build cluster

package plugin_cluster

import (
	"errors"
	"sync"

	m7s "m7s.live/v5"
	plugin_mp4 "m7s.live/v5/plugin/mp4"
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

	// stopRelayHook 是 Phase 3 Relay 在 origin 失联(§4.2)时 Stop 本节点 cluster-relay
	// pull-proxy 的注入点。默认 nil → 生产实现走 Server pull-proxy 查找 + Stop。
	stopRelayHook func(streamPath string, reason error)

	// activeRelays 跟踪本节点上 cluster-relay 派生的 streamPath。OnSubscribe
	// 成功调 relayHook 后写入;StreamRegistry.AddOnStreamRemoved 看到删除时读
	// 来决定是否 Stop。
	activeRelaysMu sync.Mutex
	activeRelays   map[string]struct{}
	relayHooksOnce sync.Once
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
	p.setupRelayHooks()
	if err := p.AddTask(&peerSyncTask{plugin: p}).WaitStarted(); err != nil {
		return err
	}

	// Phase 5A: 注入 NodeID 回调到 m7s 核心(Recorder 写库时填 node_id 列)。
	m7s.SetNodeIDHook(func() string { return p.NodeID })

	// Phase 5A: 注入 DownloadHook 到 mp4 插件。
	// /download 收到请求时,若本节点的录像注册表中没有该流,
	// 302 重定向到 origin 节点的 advertise.FLV 地址前缀。
	plugin_mp4.DownloadHook = func(streamPath string) (string, bool) {
		if p.streamRegistry == nil || p.membership == nil {
			return "", false
		}
		owner, ok := p.streamRegistry.Lookup(streamPath)
		if !ok || owner == p.NodeID {
			return "", false
		}
		peer, peerOk := p.membership.Peer(owner)
		if !peerOk || peer.Advertise.FLV == "" {
			return "", false
		}
		return peer.Advertise.FLV, true
	}

	// Phase 5 P4 决策: cluster 启用 + SQLite 仅 Warn,不拒启。
	if p.DB != nil {
		if dialect := p.DB.Dialector.Name(); dialect == "sqlite" {
			p.Warn("cluster + SQLite 不建议生产用",
				"reason", "跨节点录制元数据(mp4_streams)不会自动共享。建议切到 PostgreSQL(P4)")
		}
	}

	return nil
}

// setupRelayHooks 把 StreamRegistry 的 onStreamRemoved 回调和 activeRelays 初始化
// 绑定在一起。使用 sync.Once 保证只注册一次,可从 Start() 或 ensureRelay 调用。
func (p *ClusterPlugin) setupRelayHooks() {
	p.relayHooksOnce.Do(func() {
		p.activeRelaysMu.Lock()
		if p.activeRelays == nil {
			p.activeRelays = make(map[string]struct{})
		}
		p.activeRelaysMu.Unlock()
		if p.streamRegistry == nil {
			return
		}
		p.streamRegistry.AddOnStreamRemoved(func(streamPath string) {
			p.activeRelaysMu.Lock()
			_, isActive := p.activeRelays[streamPath]
			delete(p.activeRelays, streamPath)
			p.activeRelaysMu.Unlock()
			if !isActive {
				return
			}
			if hook := p.stopRelayHook; hook != nil {
				hook(streamPath, ErrOriginLost)
				return
			}
			p.stopRelayPullProxy(streamPath, ErrOriginLost)
		})
		p.streamRegistry.SetOnStopPublisher(func(streamPath string, reason error) {
			if p.Server == nil {
				return
			}
			pub, ok := p.Server.Streams.SafeGet(streamPath)
			if !ok {
				return
			}
			pub.Stop(reason)
		})
	})
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
