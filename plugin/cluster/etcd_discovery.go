package plugin_claster

import (
	"context"
	"fmt"
	"path"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gopkg.in/yaml.v2"
	"m7s.live/v5/pkg/task"
	"m7s.live/v5/plugin/cluster/pb"
)

// EtcdDiscovery 实现基于 etcd 的服务发现
type EtcdDiscovery struct {
	task.Work
	plugin    *ClusterPlugin
	client    *clientv3.Client
	leaseID   clientv3.LeaseID
	keyPrefix string
	nodePath  string
	watchChan clientv3.WatchChan
}

// WatchTask 监控任务
type WatchTask struct {
	task.ChannelTask
	discovery *EtcdDiscovery
}

// GetSignal 获取信号通道
func (t *WatchTask) GetSignal() any {
	return t.discovery.watchChan
}

// Tick 处理监控事件
func (t *WatchTask) Tick(resp any) {
	watchResp := resp.(clientv3.WatchResponse)
	if watchResp.Err() != nil {
		t.discovery.Error("Watch", "error", watchResp.Err())
		return
	}
	t.discovery.handleWatchEvents(watchResp.Events)
}

// NewEtcdDiscovery 创建新的 etcd 服务发现实例
func NewEtcdDiscovery(plugin *ClusterPlugin) (*EtcdDiscovery, error) {

	// 创建 etcd 客户端配置
	config := clientv3.Config{
		Endpoints:   plugin.Etcd.Endpoints,
		DialTimeout: plugin.Etcd.DialTimeout,
		Username:    plugin.Etcd.Username,
		Password:    plugin.Etcd.Password,
	}

	plugin.Info("Creating etcd client", "endpoints", config.Endpoints, "dialTimeout", config.DialTimeout)

	// 创建 etcd 客户端
	client, err := clientv3.New(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create etcd client: %v", err)
	}

	// 设置键前缀
	if plugin.Etcd.KeyPrefix == "" {
		plugin.Etcd.KeyPrefix = "/m7s/cluster"
	}

	// 构建节点路径
	nodePath := path.Join(plugin.Etcd.KeyPrefix, "nodes", plugin.NodeID)
	plugin.Info("Node path", "path", nodePath)

	return &EtcdDiscovery{
		plugin:    plugin,
		client:    client,
		keyPrefix: plugin.Etcd.KeyPrefix,
		nodePath:  nodePath,
	}, nil
}

// Start 启动服务发现
func (d *EtcdDiscovery) Start() error {
	d.Info("Starting etcd discovery", "endpoints", d.plugin.Etcd.Endpoints)

	// 创建租约
	d.Info("Creating lease with TTL", "ttl", d.plugin.Etcd.NodeKeyTTL)
	lease, err := d.client.Grant(d.plugin, d.plugin.Etcd.NodeKeyTTL)
	if err != nil {
		return fmt.Errorf("failed to create lease: %v", err)
	}
	d.leaseID = lease.ID
	d.Info("Lease created successfully", "leaseID", d.leaseID)

	// 注册节点信息
	if err := d.registerNode(); err != nil {
		return fmt.Errorf("failed to register node: %v", err)
	}

	// 启动租约保持
	d.keepAlive()

	// 同步初始节点信息
	if err := d.syncNodes(); err != nil {
		d.Error("Failed to sync initial nodes", "error", err)
	}

	// 如果启用了监控，开始监控其他节点
	if d.plugin.Etcd.EnableWatcher {
		d.startWatch()
		// 启动监控任务
		watchTask := &WatchTask{discovery: d}
		d.AddTask(watchTask)
	}

	return nil
}

// Dispose 清理资源
func (d *EtcdDiscovery) Dispose() {
	// 删除节点信息
	if d.leaseID != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		d.client.Revoke(ctx, d.leaseID)
	}

	// 关闭客户端连接
	if d.client != nil {
		d.client.Close()
	}
}

// registerNode 注册节点信息
func (d *EtcdDiscovery) registerNode() error {
	d.Info("Registering node", "nodeID", d.plugin.NodeID, "path", d.nodePath)

	// 从 nodeAgent 获取节点信息
	nodeInfo := d.plugin.nodeAgent.nodeInfo
	if nodeInfo == nil {
		return fmt.Errorf("node info not available from node agent")
	}

	data, err := yaml.Marshal(nodeInfo)
	if err != nil {
		return fmt.Errorf("failed to marshal node info: %v", err)
	}

	// // 序列化节点信息
	// data, err := json.Marshal(nodeInfo)
	// if err != nil {
	// 	return fmt.Errorf("failed to marshal node info: %v", err)
	// }

	d.Info("Node info", "data", string(data))

	// 写入 etcd
	ctx, cancel := context.WithTimeout(context.Background(), d.plugin.Etcd.RequestTimeout)
	defer cancel()

	d.Info("Putting node info to etcd", "path", d.nodePath)
	_, err = d.client.Put(ctx, d.nodePath, string(data), clientv3.WithLease(d.leaseID))
	if err != nil {
		return fmt.Errorf("failed to put node info: %v", err)
	}

	d.Info("Node info put successfully")

	return nil
}

// keepAlive 保持租约
func (d *EtcdDiscovery) keepAlive() {
	d.Info("Starting lease keepalive", "leaseID", d.leaseID)

	// 创建租约保持通道
	ch, err := d.client.KeepAlive(d.Context, d.leaseID)
	if err != nil {
		d.Error("Failed to keep lease alive", "error", err)
		return
	}

	// 处理租约保持响应
	go func() {
		for resp := range ch {
			if resp == nil {
				d.Error("Lease keepalive channel closed")
				return
			}
			d.Debug("Lease keepalive successful", "leaseID", d.leaseID, "ttl", resp.TTL)
		}
		d.Info("Lease keepalive stopped", "leaseID", d.leaseID)
	}()
}

// startWatch 开始监控其他节点
func (d *EtcdDiscovery) startWatch() {
	d.Info("Starting node watcher", "path", path.Join(d.keyPrefix, "nodes"))

	// 监控节点目录
	watchPath := path.Join(d.keyPrefix, "nodes")
	d.watchChan = d.client.Watch(d, watchPath, clientv3.WithPrefix())
	d.Info("Node watcher started successfully")
}

// syncNodes 同步节点信息
func (d *EtcdDiscovery) syncNodes() error {
	d.Info("开始从 etcd 同步节点",
		"currentNodesLength", d.plugin.nodes.Length,
		"currentTotalNodes", d.plugin.TotalNodes,
	)

	// 获取所有节点信息
	ctx, cancel := context.WithTimeout(context.Background(), d.plugin.Etcd.RequestTimeout)
	defer cancel()

	// 查询节点目录
	watchPath := path.Join(d.keyPrefix, "nodes")
	d.Info("Querying etcd for nodes", "path", watchPath)

	resp, err := d.client.Get(ctx, watchPath, clientv3.WithPrefix())
	if err != nil {
		return fmt.Errorf("failed to get nodes: %v", err)
	}

	d.Info("Found nodes in etcd", "count", len(resp.Kvs))

	// 更新节点集合
	for _, kv := range resp.Kvs {
		d.Info("Processing node", "key", string(kv.Key))

		var nodeInfo NodeInfo
		if err := yaml.Unmarshal(kv.Value, &nodeInfo); err != nil {
			d.Error("Failed to unmarshal node info:", "error", err, "key", string(kv.Key))
			continue
		}

		// 跳过本地节点
		if nodeInfo.ID == d.plugin.NodeID {
			d.Info("Skipping local node update", "nodeID", nodeInfo.ID)
			continue
		}
		nodeInfo.LastHeartbeat = time.Now()
		// 创建节点客户端
		if nodeInfo.IP != "" {
			// 创建 API 客户端，用于节点间通信
			if nodeInfo.ApiClient == nil && nodeInfo.ID != d.plugin.NodeID {
				nodeURL := nodeInfo.IP
				if nodeInfo.Port > 0 {
					nodeURL = fmt.Sprintf("%s:%d", nodeInfo.IP, nodeInfo.Port)
				}

				// 创建 gRPC 连接
				conn, err := grpc.NewClient(nodeURL, grpc.WithTransportCredentials(insecure.NewCredentials()))
				if err != nil {
					d.Error("Failed to create API client for node", "nodeID", nodeInfo.ID, "error", err)
				} else {
					nodeInfo.ApiClient = pb.NewApiClient(conn)
					d.Info("Created API client for node", "nodeID", nodeInfo.ID, "nodeURL", nodeURL)
				}
			}
		}

		// 更新节点集合
		d.plugin.nodes.Set(&nodeInfo)
		d.Info("Updated node in local collection", "nodeID", nodeInfo.ID, "status", nodeInfo.Status)
	}

	d.Info("Node sync completed", "totalNodes", d.plugin.nodes.Length)
	return nil
}

// handleWatchEvents 处理监控事件
func (d *EtcdDiscovery) handleWatchEvents(events []*clientv3.Event) {
	d.Info("Handling watch events", "count", len(events))

	for _, event := range events {
		switch event.Type {
		case clientv3.EventTypePut:
			// 节点添加或更新
			var nodeInfo NodeInfo
			if err := yaml.Unmarshal(event.Kv.Value, &nodeInfo); err != nil {
				d.Error("Failed to unmarshal node info", "error", err)
				continue
			}
			d.Info("Node added/updated", "nodeID", nodeInfo.ID, "role", nodeInfo.Role, "status", nodeInfo.Status)

			// 创建节点客户端
			if nodeInfo.IP != "" && nodeInfo.ID != d.plugin.NodeID {
				// 创建 API 客户端，用于节点间通信
				if nodeInfo.ApiClient == nil {
					nodeURL := nodeInfo.IP
					if nodeInfo.Port > 0 {
						nodeURL = fmt.Sprintf("%s:%d", nodeInfo.IP, nodeInfo.Port)
					}

					// 创建 gRPC 连接
					conn, err := grpc.NewClient(nodeURL, grpc.WithTransportCredentials(insecure.NewCredentials()))
					if err != nil {
						d.Error("Failed to create API client for node", "nodeID", nodeInfo.ID, "error", err)
					} else {
						nodeInfo.ApiClient = pb.NewApiClient(conn)
						d.Info("Created API client for node", "nodeID", nodeInfo.ID, "nodeURL", nodeURL)
					}
				}
			}

			d.plugin.nodes.Set(&nodeInfo)

		case clientv3.EventTypeDelete:
			// 节点删除
			nodeID := path.Base(string(event.Kv.Key))
			d.Info("Node deleted", "nodeID", nodeID)
			d.plugin.nodes.RemoveByKey(nodeID)
		}
	}
}

// RemoveNode 从 etcd 中移除节点
func (d *EtcdDiscovery) RemoveNode(nodeID string) error {
	nodePath := path.Join(d.keyPrefix, "nodes", nodeID)
	d.Info("Removing node from etcd", "nodeID", nodeID, "path", nodePath)

	ctx, cancel := context.WithTimeout(context.Background(), d.plugin.Etcd.RequestTimeout)
	defer cancel()

	_, err := d.client.Delete(ctx, nodePath)
	if err != nil {
		return fmt.Errorf("failed to delete node: %v", err)
	}

	d.Info("Node removed successfully", "nodeID", nodeID)
	return nil
}
