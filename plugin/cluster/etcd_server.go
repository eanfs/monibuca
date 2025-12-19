package plugin_claster

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"go.etcd.io/etcd/server/v3/embed"
	"m7s.live/v5/pkg/task"
)

// EtcdServer 内嵌的 etcd 服务器
type EtcdServer struct {
	task.ChannelTask
	plugin *ClusterPlugin
	etcd   *embed.Etcd
	cfg    *embed.Config
}

// GetSignal 返回错误通道作为信号
func (s *EtcdServer) GetSignal() any {
	return s.etcd.Err()
}

// NewEtcdServer 创建新的 etcd 服务器实例
func NewEtcdServer(plugin *ClusterPlugin) (*EtcdServer, error) {
	if !plugin.Etcd.Server.Enabled {
		return nil, fmt.Errorf("etcd server is not enabled")
	}

	plugin.Info("Creating new etcd server instance")

	// 创建 etcd 配置
	cfg := embed.NewConfig()
	plugin.Info("Created new etcd config")

	// 设置数据目录
	if plugin.Etcd.Server.DataDir == "" {
		plugin.Etcd.Server.DataDir = "data/etcd"
	}
	dataDir := filepath.Join(plugin.Etcd.Server.DataDir, plugin.NodeID)
	cfg.Dir = dataDir
	plugin.Info("Setting data directory", "path", dataDir)

	// 确保数据目录存在
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		plugin.Error("Failed to create data directory", "error", err)
		return nil, fmt.Errorf("failed to create data directory: %v", err)
	}
	plugin.Info("Data directory created successfully")

	// 设置监听地址
	if len(plugin.Etcd.Server.ListenClientUrls) > 0 {
		urls := make([]url.URL, 0, len(plugin.Etcd.Server.ListenClientUrls))
		for _, addr := range plugin.Etcd.Server.ListenClientUrls {
			u, err := url.Parse(addr)
			if err != nil {
				plugin.Error("Invalid listen client URL", "url", addr, "error", err)
				return nil, fmt.Errorf("invalid listen client url: %v", err)
			}
			urls = append(urls, *u)
		}
		cfg.ListenClientUrls = urls
		plugin.Info("Setting listen client URLs", "urls", urls)
	} else {
		// 默认监听地址
		cfg.ListenClientUrls = []url.URL{{Scheme: "http", Host: "localhost:2379"}}
		plugin.Info("Using default listen client URL", "url", cfg.ListenClientUrls)
	}

	// 设置对外公布的客户端地址
	if len(plugin.Etcd.Server.AdvertiseClientUrls) > 0 {
		urls := make([]url.URL, 0, len(plugin.Etcd.Server.AdvertiseClientUrls))
		for _, addr := range plugin.Etcd.Server.AdvertiseClientUrls {
			u, err := url.Parse(addr)
			if err != nil {
				plugin.Error("Invalid advertise client URL", "url", addr, "error", err)
				return nil, fmt.Errorf("invalid advertise client url: %v", err)
			}
			urls = append(urls, *u)
		}
		cfg.AdvertiseClientUrls = urls
		plugin.Info("Setting advertise client URLs", "urls", urls)
	} else {
		cfg.AdvertiseClientUrls = cfg.ListenClientUrls
		plugin.Info("Using listen client URLs as advertise client URLs")
	}

	// 设置节点间通信地址
	if len(plugin.Etcd.Server.ListenPeerUrls) > 0 {
		urls := make([]url.URL, 0, len(plugin.Etcd.Server.ListenPeerUrls))
		for _, addr := range plugin.Etcd.Server.ListenPeerUrls {
			u, err := url.Parse(addr)
			if err != nil {
				plugin.Error("Invalid listen peer URL", "url", addr, "error", err)
				return nil, fmt.Errorf("invalid listen peer url: %v", err)
			}
			urls = append(urls, *u)
		}
		cfg.ListenPeerUrls = urls
		plugin.Info("Setting listen peer URLs", "urls", urls)
	}

	// 设置对外公布的节点间通信地址
	if len(plugin.Etcd.Server.AdvertisePeerUrls) > 0 {
		urls := make([]url.URL, 0, len(plugin.Etcd.Server.AdvertisePeerUrls))
		for _, addr := range plugin.Etcd.Server.AdvertisePeerUrls {
			u, err := url.Parse(addr)
			if err != nil {
				plugin.Error("Invalid advertise peer URL", "url", addr, "error", err)
				return nil, fmt.Errorf("invalid advertise peer url: %v", err)
			}
			urls = append(urls, *u)
		}
		cfg.AdvertisePeerUrls = urls
		plugin.Info("Setting advertise peer URLs", "urls", urls)
	} else if len(cfg.ListenPeerUrls) > 0 {
		cfg.AdvertisePeerUrls = cfg.ListenPeerUrls
		plugin.Info("Using listen peer URLs as advertise peer URLs")
	}

	// 设置初始集群配置
	if plugin.Etcd.Server.InitialCluster != "" {
		cfg.InitialCluster = plugin.Etcd.Server.InitialCluster
		plugin.Info("Setting initial cluster", "cluster", cfg.InitialCluster)
	} else {
		cfg.InitialCluster = fmt.Sprintf("%s=http://localhost:2380", plugin.NodeID)
		plugin.Info("Using default initial cluster", "cluster", cfg.InitialCluster)
	}

	// 设置初始集群状态
	if plugin.Etcd.Server.InitialClusterState != "" {
		cfg.ClusterState = plugin.Etcd.Server.InitialClusterState
		plugin.Info("Setting initial cluster state", "state", cfg.ClusterState)
	}

	// 设置集群 token
	if plugin.Etcd.Server.InitialClusterToken != "" {
		cfg.InitialClusterToken = plugin.Etcd.Server.InitialClusterToken
		plugin.Info("Setting initial cluster token", "token", cfg.InitialClusterToken)
	}

	// 设置快照配置
	if plugin.Etcd.Server.SnapshotCount > 0 {
		cfg.SnapshotCount = plugin.Etcd.Server.SnapshotCount
		plugin.Info("Setting snapshot count", "count", cfg.SnapshotCount)
	}

	// 设置自动压缩
	if plugin.Etcd.Server.AutoCompactionMode != "" {
		cfg.AutoCompactionMode = plugin.Etcd.Server.AutoCompactionMode
		cfg.AutoCompactionRetention = plugin.Etcd.Server.AutoCompactionRetention
		plugin.Info("Setting auto compaction", "mode", cfg.AutoCompactionMode, "retention", cfg.AutoCompactionRetention)
	}

	// 设置配额
	if plugin.Etcd.Server.QuotaBackendBytes > 0 {
		cfg.QuotaBackendBytes = plugin.Etcd.Server.QuotaBackendBytes
		plugin.Info("Setting quota backend bytes", "bytes", cfg.QuotaBackendBytes)
	}

	// 设置名称
	cfg.Name = plugin.NodeID
	plugin.Info("Setting node name", "name", cfg.Name)

	plugin.Info("Etcd server configuration complete",
		"name", cfg.Name,
		"dir", cfg.Dir,
		"listenClientUrls", cfg.ListenClientUrls,
		"advertiseClientUrls", cfg.AdvertiseClientUrls,
		"listenPeerUrls", cfg.ListenPeerUrls,
		"advertisePeerUrls", cfg.AdvertisePeerUrls,
		"initialCluster", cfg.InitialCluster,
		"clusterState", cfg.ClusterState,
		"initialClusterToken", cfg.InitialClusterToken,
	)

	server := &EtcdServer{
		plugin: plugin,
		cfg:    cfg,
	}
	return server, nil
}

// Start 启动 etcd 服务器
func (s *EtcdServer) Start() error {
	s.plugin.Info("Starting etcd server...")

	// 启动 etcd 服务器
	e, err := embed.StartEtcd(s.cfg)
	if err != nil {
		s.plugin.Error("Failed to start etcd server", "error", err)
		return fmt.Errorf("failed to start etcd server: %v", err)
	}
	s.plugin.Info("Etcd server started successfully")

	// 保存 etcd 实例
	s.etcd = e

	// 等待服务器就绪
	select {
	case <-e.Server.ReadyNotify():
		s.plugin.Info("Etcd server is ready")
	case <-time.After(60 * time.Second):
		e.Server.Stop() // 触发关闭
		s.plugin.Error("Etcd server took too long to start")
		return fmt.Errorf("etcd server took too long to start")
	case err := <-e.Err():
		s.plugin.Error("Etcd server error during startup", "error", err)
		return fmt.Errorf("etcd server error during startup: %v", err)
	}

	s.plugin.Info("Etcd server startup complete",
		"clientURL", s.cfg.ListenClientUrls,
		"peerURL", s.cfg.ListenPeerUrls,
	)

	return nil
}

// Tick 处理错误信号
func (s *EtcdServer) Tick(err any) {
	if err != nil {
		s.plugin.Error("Etcd server error", "error", err)
	}
}

// Dispose 清理资源
func (s *EtcdServer) Dispose() {
	if s.etcd != nil {
		s.etcd.Close()
	}
}
