package plugin_claster

import (
	"context"
	"fmt"
	"sync"
	"time"

	"m7s.live/v5"
	"m7s.live/v5/pkg"
	"m7s.live/v5/pkg/task"
	"m7s.live/v5/plugin/cluster/pb"
)

var _ = m7s.InstallPlugin[ClusterPlugin](&pb.Api_ServiceDesc, pb.RegisterApiHandler)
var _ pb.ApiServer = (*ClusterPlugin)(nil)

// ClusterPlugin 集群插件
type ClusterPlugin struct {
	m7s.Plugin
	pb.UnimplementedApiServer
	NodeID              string
	Role                string
	Region              string
	SyncInterval        time.Duration `default:"5s"`
	HeartbeatInterval   time.Duration `default:"2s"`
	HealthCheckInterval time.Duration `default:"2s"`
	ClusterSecret       string
	AuthToken           string
	ManagerAddress      string

	OfflineThreshold time.Duration `default:"5s"`  // 离线判定阈值
	FailoverDelay    time.Duration `default:"10s"` // 故障转移延迟时间

	Resources     ResourceConfig
	LoadBalancing LoadBalancingConfig
	Monitoring    MonitoringConfig
	Sync          SyncConfig
	Etcd          EtcdConfig

	nodes         task.Manager[string, *NodeInfo]
	streams       task.Manager[string, *StreamInfo]
	nodeAgent     *NodeAgent
	loadBalancer  ILoadBalancer
	managerClient *ManagerClient

	// Components
	resourceOptimizer *ResourceOptimizer
	healthMonitor     *HealthMonitor
	stateMonitor      *ClusterStateMonitor
	streamSyncService *StreamSyncService
	etcdStreamSync    *EtcdStreamSync

	// 集群状态
	ClusterState string
	TotalNodes   int
	HealthyNodes int
	RoleCounts   map[string]int
}

// OnInit 初始化插件
func (p *ClusterPlugin) OnInit() error {
	if p.Role == "" {
		p.Role = "manager"
	}
	if p.NodeID == "" {
		p.NodeID = generateNodeID(p)
	}
	p.Info("Initializing cluster plugin",
		"role", p.Role,
		"nodeId", p.NodeID,
		"etcdEnabled", p.Etcd.Enabled,
	)
	p.streams.L = &sync.RWMutex{}
	p.nodes.L = &sync.RWMutex{}
	// 初始化集群状态
	p.ClusterState = "normal"
	p.TotalNodes = 0
	p.HealthyNodes = 0
	p.RoleCounts = make(map[string]int)

	// 初始化组件
	p.loadBalancer = NewLoadBalancer(p)
	p.resourceOptimizer = NewResourceOptimizer(p)
	p.healthMonitor = NewHealthMonitor(p)
	p.stateMonitor = NewClusterStateMonitor(p)
	p.streamSyncService = NewStreamSyncService(p)

	// 启动组件
	p.AddTask(p.resourceOptimizer)
	p.AddTask(p.healthMonitor)
	p.AddTask(p.stateMonitor)
	p.AddTask(p.streamSyncService)

	// 如果启用了 etcd
	if p.Etcd.Enabled {
		// 如果是 etcd 模式且是管理节点，启动 etcd 服务器
		if p.Role == "manager" {
			// 初始化 etcd 服务器
			server, err := NewEtcdServer(p)
			if err != nil {
				p.Error("Failed to create etcd server", "error", err)
				return fmt.Errorf("failed to create etcd server: %v", err)
			}

			err = p.AddTask(server).WaitStarted()
			if err != nil {
				p.Error("Failed to start etcd server", "error", err)
				return err
			}
		}

		// 初始化节点代理
		p.nodeAgent = NewNodeAgent(p)
		err := p.AddTask(p.nodeAgent).WaitStarted()
		if err != nil {
			p.Error("Failed to start node agent", "error", err)
			return err
		}
		p.Info("Node agent initialized successfully")

		// 创建 etcd 发现实例
		discovery, err := NewEtcdDiscovery(p)
		if err != nil {
			p.Error("Failed to create etcd discovery", "error", err)
			return fmt.Errorf("failed to create etcd discovery: %v", err)
		}
		err = p.AddTask(discovery).WaitStarted()
		if err != nil {
			p.Error("Failed to start etcd discovery", "error", err)
			return err
		}
		p.Info("Etcd discovery instance created")

		// 创建 etcd 流同步实例
		p.etcdStreamSync = NewEtcdStreamSync(p, discovery.client)
		err = p.AddTask(p.etcdStreamSync).WaitStarted()
		if err != nil {
			p.Error("Failed to start etcd stream sync", "error", err)
			return err
		}
		p.Info("Etcd stream sync instance created")

		// 如果不是管理节点，创建并启动 manager client
		if p.Role != "manager" && p.ManagerAddress != "" {
			managerClient := NewManagerClient(p, p.ManagerAddress)
			err = p.AddTask(managerClient).WaitStarted()
			if err != nil {
				p.Error("Failed to start manager client", "error", err)
				return err
			}
			p.Info("Manager client initialized successfully")
		}
	}

	p.Info("Cluster plugin initialized successfully")
	return nil
}

// RegisterStreamInternal registers a new stream with the cluster
func (p *ClusterPlugin) RegisterStreamInternal(streamInfo *StreamInfo) error {
	p.Info("Registering stream", "streamPath", streamInfo.StreamPath)

	// 创建一个背景上下文作为默认上下文
	backgroundCtx := context.Background()

	// 确保流信息有一个有效的上下文
	if streamInfo.Context == nil {
		// 如果没有上下文，使用背景上下文
		streamInfo.Context = backgroundCtx
	}

	// Check if the stream already exists
	existingStream, exists := p.streams.Get(streamInfo.StreamPath)
	if exists {
		// Check for conflicting publisher nodes
		if existingStream.PublisherNodeID != streamInfo.PublisherNodeID {
			p.Warn("Stream already published by different node",
				"streamPath", streamInfo.StreamPath,
				"existingPublisher", existingStream.PublisherNodeID)

			// Check vector clocks to resolve conflict
			if p.IsNewerStreamVersion(streamInfo, existingStream) {
				p.Info("New publisher has newer version, updating stream info",
					"streamPath", streamInfo.StreamPath)
			} else {
				// Reject the new publisher if the existing one has precedence
				errMsg := fmt.Sprintf("Stream %s already published by node %s with higher precedence",
					streamInfo.StreamPath, existingStream.PublisherNodeID)
				p.Error("Stream registration rejected",
					"streamPath", streamInfo.StreamPath,
					"existingPublisher", existingStream.PublisherNodeID,
					"reason", errMsg)
				return fmt.Errorf(errMsg)
			}
		}

		// Update stream information
		streamInfo.LastUpdated = time.Now()
		// Preserve replicated nodes list if not provided in the new info
		if len(streamInfo.ReplicatedTo) == 0 && len(existingStream.ReplicatedTo) > 0 {
			streamInfo.ReplicatedTo = existingStream.ReplicatedTo
		}
	} else {
		// Initialize new stream
		streamInfo.LastUpdated = time.Now()
		streamInfo.State = "active"
		if streamInfo.VectorClock == nil {
			streamInfo.VectorClock = make(map[string]uint64)
		}
		streamInfo.VectorClock[streamInfo.PublisherNodeID] = 1
	}

	// Store the stream
	p.streams.Add(streamInfo, p.Logger, streamInfo.Context)

	// Notify components
	p.loadBalancer.OnStreamAdded(streamInfo)
	p.resourceOptimizer.OnStreamAdded(streamInfo)

	// 如果启用了 etcd，注册流到 etcd
	if p.Etcd.Enabled && p.etcdStreamSync != nil {
		if err := p.etcdStreamSync.RegisterStream(streamInfo); err != nil {
			p.Error("Failed to register stream to etcd", "error", err)
		}
	}

	// Update node's stream count
	nodeInfo, nodeExists := p.nodes.Get(streamInfo.PublisherNodeID)
	if nodeExists {
		// Count streams for this node
		count := 0
		p.streams.Range(func(stream *StreamInfo) bool {
			if stream.PublisherNodeID == streamInfo.PublisherNodeID {
				count++
			}
			return true
		})
		nodeInfo.StreamCount = count
	}

	p.Info("Stream registered successfully", "streamPath", streamInfo.StreamPath)
	return nil
}

// IsNewerStreamVersion compares vector clocks to determine if the new stream info is newer
func (p *ClusterPlugin) IsNewerStreamVersion(newStream, existingStream *StreamInfo) bool {
	// If the new stream has no vector clock, it can't be newer
	if len(newStream.VectorClock) == 0 {
		return false
	}

	// If the existing stream has no vector clock, the new one is newer
	if len(existingStream.VectorClock) == 0 {
		return true
	}

	// Check if any entry in the new clock is greater than in the existing clock
	hasGreater := false
	for node, count := range newStream.VectorClock {
		existingCount, exists := existingStream.VectorClock[node]
		if !exists || count > existingCount {
			hasGreater = true
		} else if count < existingCount {
			// If any entry is less, there's a conflict
			return false
		}
	}

	return hasGreater
}

// UnregisterStreamInternal removes a stream from the cluster
func (p *ClusterPlugin) UnregisterStreamInternal(streamPath string) error {
	p.Info("Unregistering stream", "streamPath", streamPath)

	// Get stream info
	streamInfo, ok := p.streams.Get(streamPath)
	if !ok {
		return fmt.Errorf("stream not found: %s", streamPath)
	}

	// Notify components
	p.loadBalancer.OnStreamRemoved(streamInfo)
	p.resourceOptimizer.OnStreamRemoved(streamInfo)

	// 如果启用了 etcd，从 etcd 注销流
	if p.Etcd.Enabled && p.etcdStreamSync != nil {
		if err := p.etcdStreamSync.UnregisterStream(streamPath); err != nil {
			p.Error("Failed to unregister stream from etcd", "error", err)
		}
	}

	// Remove from streams collection
	p.streams.RemoveByKey(streamPath)

	return nil
}

// UpdateNodeStatusInternal updates the status of a node
func (p *ClusterPlugin) UpdateNodeStatusInternal(nodeID string, status string) error {
	// Get node info
	nodeInfo, ok := p.nodes.Get(nodeID)
	if !ok {
		return fmt.Errorf("node not found: %s", nodeID)
	}

	// Update status
	oldStatus := nodeInfo.Status
	nodeInfo.Status = status

	// Log status change
	if oldStatus != status {
		p.Info("Node status changed", "nodeID", nodeID, "oldStatus", oldStatus, "newStatus", status)
	}

	return nil
}

// GetOptimalNodeForPublisher returns the optimal node for a new publisher
func (p *ClusterPlugin) GetOptimalNodeForPublisher(ctx context.Context, req *pb.GetOptimalNodeForPublisherRequest) (*pb.GetOptimalNodeForPublisherResponse, error) {
	node, err := p.loadBalancer.GetOptimalNodeForPublisher()
	if err != nil {
		return nil, err
	}

	// 准备响应数据
	responseData := &pb.GetOptimalNodeForPublisherResponseData{
		OptimalNode: &pb.NodeInfo{
			Id:            node.ID,
			Role:          node.Role,
			Region:        node.Region,
			Status:        node.Status,
			LastHeartbeat: node.LastHeartbeat.UnixMilli(),
			Capacity:      convertInternalResourceCapacityToProto(&node.Capacity),
			StreamCount:   int32(node.StreamCount),
		},
		FallbackNodes: nil,
	}

	return &pb.GetOptimalNodeForPublisherResponse{
		Code:    0,
		Message: "success",
		Data:    responseData,
	}, nil
}

// GetOptimalNodeForSubscriber returns the optimal node for a new subscriber
func (p *ClusterPlugin) GetOptimalNodeForSubscriber(ctx context.Context, req *pb.GetOptimalNodeForSubscriberRequest) (*pb.GetOptimalNodeForSubscriberResponse, error) {
	node, err := p.loadBalancer.GetOptimalNodeForSubscriber(req.StreamPath)
	if err != nil {
		return nil, err
	}
	// 准备响应数据
	responseData := &pb.GetOptimalNodeForSubscriberResponseData{
		OptimalNode: &pb.NodeInfo{
			Id:            node.ID,
			Role:          node.Role,
			Region:        node.Region,
			Status:        node.Status,
			LastHeartbeat: node.LastHeartbeat.UnixMilli(),
			Capacity:      convertInternalResourceCapacityToProto(&node.Capacity),
			StreamCount:   int32(node.StreamCount),
		},
	}

	return &pb.GetOptimalNodeForSubscriberResponse{
		Code:    0,
		Message: "success",
		Data:    responseData,
	}, nil
}

// OnPublish handles stream publish events
func (p *ClusterPlugin) OnPublish(pub *m7s.Publisher) {
	// 创建一个背景上下文作为默认上下文
	backgroundCtx := context.Background()

	// Get stream info from publisher
	streamInfo := &StreamInfo{
		StreamPath:      pub.StreamPath,
		PublisherNodeID: p.NodeID,
		ReplicatedTo:    make([]string, 0),
		SubscriberCount: pub.Subscribers.Length,
		State:           "active",
		MediaInfo: MediaInfo{
			VideoCodec:    pub.VideoTrack.AVTrack.FourCC().String(),
			AudioCodec:    pub.AudioTrack.AVTrack.FourCC().String(),
			Resolution:    fmt.Sprintf("%dx%d", pub.VideoTrack.AVTrack.ICodecCtx.(pkg.IVideoCodecCtx).Width(), pub.VideoTrack.AVTrack.ICodecCtx.(pkg.IVideoCodecCtx).Height()),
			Framerate:     float64(pub.VideoTrack.AVTrack.FPS),
			VideoWidth:    pub.VideoTrack.AVTrack.ICodecCtx.(pkg.IVideoCodecCtx).Width(),
			VideoHeight:   pub.VideoTrack.AVTrack.ICodecCtx.(pkg.IVideoCodecCtx).Height(),
			VideoEnabled:  pub.VideoTrack.AVTrack != nil,
			AudioEnabled:  pub.AudioTrack.AVTrack != nil,
			StartTime:     pub.StartTime.Unix(),
			BandwidthMbps: float64(pub.VideoTrack.AVTrack.BPS+pub.AudioTrack.AVTrack.BPS) / 1000000,
		},
		ClientInfo: ClientInfo{
			ClientID:    fmt.Sprintf("%d", pub.ID),
			ClientIP:    pub.RemoteAddr,
			ConnectTime: pub.StartTime,
			UserAgent:   string(pub.Args.Get("User-Agent")[0]),
			Metadata:    make(map[string]string),
		},
		CreationTime: time.Now(),
		LastUpdated:  time.Now(),
		VectorClock:  make(map[string]uint64),
		Tags:         make(map[string]string),
		StartTime:    pub.StartTime,
		Context:      backgroundCtx, // 使用背景上下文
	}

	// Convert url.Values to map[string]string
	for k, v := range pub.Args {
		streamInfo.ClientInfo.Metadata[k] = v[0]
		streamInfo.Tags[k] = v[0]
	}

	// Add to local streams map
	p.streams.Add(streamInfo, p.Logger)

	// Set up dispose handler for local cleanup
	pub.OnDispose(func() {
		p.Info("Stream disposed", "streamPath", streamInfo.StreamPath)
		p.streams.Remove(streamInfo)

		// If we're the manager, notify other nodes
		if p.Role == "manager" && p.streamSyncService != nil {
			p.streamSyncService.OnStreamRemoved(streamInfo.StreamPath)
		}
	})

	// Register stream with manager if we're not the manager
	if p.Role != "manager" && p.managerClient != nil {
		req := &pb.RegisterStreamRequest{
			StreamInfo: convertInternalStreamInfoToProto(streamInfo),
			AuthToken:  p.AuthToken,
		}

		// Send to manager
		resp, err := p.managerClient.RegisterStream(context.Background(), req)
		if err != nil {
			p.Error("Failed to register stream with manager", "error", err)
			return
		}
		if resp.Code != 0 {
			p.Error("Manager rejected stream registration", "message", resp.Message)
			return
		}
		if resp.Data.ConflictDetected {
			p.Info("Stream registration conflict detected, using resolved stream info")
			// Update local stream info with resolved version
			streamInfo = convertProtoStreamInfoToInternal(resp.Data.ResolvedStream)
			p.streams.Add(streamInfo, p.Logger, streamInfo.Context)
		}

		// Update dispose handler to also unregister from manager
		pub.OnDispose(func() {
			// Unregister stream with manager
			req := &pb.UnregisterStreamRequest{
				StreamPath: streamInfo.StreamPath,
				NodeId:     p.NodeID,
				AuthToken:  p.AuthToken,
			}
			resp, err := p.managerClient.UnregisterStream(context.Background(), req)
			if err != nil {
				p.Error("Failed to unregister stream with manager", "error", err)
				return
			}
			if resp.Code != 0 {
				p.Error("Manager rejected stream unregistration", "message", resp.Message)
				return
			}
		})
	}
}
