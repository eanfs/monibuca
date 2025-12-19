package plugin_claster

import (
	"context"
	"sync"
	"time"

	"m7s.live/v5/pkg/task"
	"m7s.live/v5/plugin/cluster/pb"
)

// StreamSyncService 流同步服务
// 负责管理节点和其他节点之间的流信息同步
type StreamSyncService struct {
	task.Work
	plugin *ClusterPlugin
	mutex  sync.RWMutex

	// 上次同步时间记录
	lastSyncTime map[string]time.Time
}

// NewStreamSyncService 创建新的流同步服务
func NewStreamSyncService(plugin *ClusterPlugin) *StreamSyncService {
	return &StreamSyncService{
		plugin:       plugin,
		lastSyncTime: make(map[string]time.Time),
	}
}

// Start 启动流同步服务
func (s *StreamSyncService) Start() error {
	s.Info("Starting stream sync service")

	// 如果是管理节点，添加定期全量同步任务
	if s.plugin.Role == "manager" {
		s.AddTask(&FullSyncTask{
			service: s,
		})
	}

	return nil
}

// SyncStreamsToNode 将流信息同步到指定节点
func (s *StreamSyncService) SyncStreamsToNode(nodeID string) error {
	s.Info("Syncing streams to node", "nodeID", nodeID)

	// 获取节点信息
	nodeInfo, exists := s.plugin.nodes.Get(nodeID)
	if !exists {
		return ErrNodeNotFound
	}

	// 检查节点是否在线
	if nodeInfo.Status != "healthy" && nodeInfo.Status != "degraded" {
		return ErrNodeOffline
	}

	// 准备流信息列表
	var streamInfos []*pb.StreamInfo
	s.plugin.streams.Range(func(stream *StreamInfo) bool {
		protoStream := convertInternalStreamInfoToProto(stream)
		if protoStream != nil {
			streamInfos = append(streamInfos, protoStream)
		}
		return true
	})

	// 发送同步请求
	if nodeInfo.ApiClient != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		req := &pb.SyncStreamsRequest{
			Streams:   streamInfos,
			FullSync:  true,
			Timestamp: time.Now().UnixMilli(),
			NodeId:    s.plugin.NodeID,
			AuthToken: s.plugin.AuthToken,
		}

		resp, err := nodeInfo.ApiClient.SyncStreams(ctx, req)
		if err != nil {
			s.Error("Failed to sync streams to node", "nodeID", nodeID, "error", err)
			return err
		}

		if resp.Code != 0 {
			s.Error("Node rejected stream sync", "nodeID", nodeID, "message", resp.Message, "code", resp.Code)
			return ErrSyncRejected
		}

		// 更新最后同步时间
		s.mutex.Lock()
		s.lastSyncTime[nodeID] = time.Now()
		s.mutex.Unlock()

		s.Info("Successfully synced streams to node",
			"nodeID", nodeID,
			"streamCount", len(streamInfos),
			"acceptedCount", resp.Data.AcceptedCount,
		)
	} else {
		s.Error("Node API client not available", "nodeID", nodeID)
		return ErrNodeClientNotAvailable
	}

	return nil
}

// SyncStreamsToAllNodes 将流信息同步到所有节点
func (s *StreamSyncService) SyncStreamsToAllNodes() {
	s.Info("Syncing streams to all nodes")

	// 遍历所有节点
	s.plugin.nodes.Range(func(node *NodeInfo) bool {
		// 跳过管理节点自身
		if node.ID == s.plugin.NodeID {
			return true
		}

		// 跳过离线节点
		if node.Status != "healthy" && node.Status != "degraded" {
			return true
		}

		// 同步到节点
		go s.SyncStreamsToNode(node.ID)

		return true
	})
}

// SyncStreamAddedToAllNodes 将新增的流同步到所有节点
func (s *StreamSyncService) SyncStreamAddedToAllNodes(stream *StreamInfo) {
	s.Info("Syncing added stream to all nodes", "streamPath", stream.StreamPath)

	// 转换为 proto 格式
	protoStream := convertInternalStreamInfoToProto(stream)
	if protoStream == nil {
		s.Error("Failed to convert stream info to proto", "streamPath", stream.StreamPath)
		return
	}

	// 遍历所有节点
	s.plugin.nodes.Range(func(node *NodeInfo) bool {
		// 跳过管理节点自身
		if node.ID == s.plugin.NodeID {
			return true
		}

		// 跳过离线节点
		if node.Status != "healthy" && node.Status != "degraded" {
			return true
		}

		// 发送增量同步请求
		if node.ApiClient != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			req := &pb.SyncStreamsRequest{
				Streams:   []*pb.StreamInfo{protoStream},
				FullSync:  false,
				Timestamp: time.Now().UnixMilli(),
				NodeId:    s.plugin.NodeID,
				AuthToken: s.plugin.AuthToken,
			}

			resp, err := node.ApiClient.SyncStreams(ctx, req)
			if err != nil {
				s.Error("Failed to sync added stream to node", "nodeID", node.ID, "error", err)
				return true
			}

			if resp.Code != 0 {
				s.Error("Node rejected stream sync", "nodeID", node.ID, "message", resp.Message, "code", resp.Code)
				return true
			}

			s.Info("Successfully synced added stream to node",
				"nodeID", node.ID,
				"streamPath", stream.StreamPath,
			)
		}

		return true
	})
}

// SyncStreamRemovedToAllNodes 将删除的流同步到所有节点
func (s *StreamSyncService) SyncStreamRemovedToAllNodes(streamPath string) {
	s.Info("Syncing removed stream to all nodes", "streamPath", streamPath)

	// 遍历所有节点
	s.plugin.nodes.Range(func(node *NodeInfo) bool {
		// 跳过管理节点自身
		if node.ID == s.plugin.NodeID {
			return true
		}

		// 跳过离线节点
		if node.Status != "healthy" && node.Status != "degraded" {
			return true
		}

		// 发送删除流请求
		if node.ApiClient != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			req := &pb.RemoveStreamRequest{
				StreamPath: streamPath,
				Timestamp:  time.Now().UnixMilli(),
				NodeId:     s.plugin.NodeID,
				AuthToken:  s.plugin.AuthToken,
			}

			resp, err := node.ApiClient.RemoveStream(ctx, req)
			if err != nil {
				s.Error("Failed to sync removed stream to node", "nodeID", node.ID, "error", err)
				return true
			}

			if resp.Code != 0 {
				s.Error("Node rejected stream removal", "nodeID", node.ID, "message", resp.Message, "code", resp.Code)
				return true
			}

			s.Info("Successfully synced removed stream to node",
				"nodeID", node.ID,
				"streamPath", streamPath,
			)
		}

		return true
	})
}

// SyncStreamUpdatedToAllNodes 将更新的流同步到所有节点
func (s *StreamSyncService) SyncStreamUpdatedToAllNodes(stream *StreamInfo) {
	s.Info("Syncing updated stream to all nodes", "streamPath", stream.StreamPath)

	// 转换为 proto 格式
	protoStream := convertInternalStreamInfoToProto(stream)
	if protoStream == nil {
		s.Error("Failed to convert stream info to proto", "streamPath", stream.StreamPath)
		return
	}

	// 遍历所有节点
	s.plugin.nodes.Range(func(node *NodeInfo) bool {
		// 跳过管理节点自身
		if node.ID == s.plugin.NodeID {
			return true
		}

		// 跳过离线节点
		if node.Status != "healthy" && node.Status != "degraded" {
			return true
		}

		// 发送更新流请求
		if node.ApiClient != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			req := &pb.SyncStreamsRequest{
				Streams:   []*pb.StreamInfo{protoStream},
				FullSync:  false,
				Timestamp: time.Now().UnixMilli(),
				NodeId:    s.plugin.NodeID,
				AuthToken: s.plugin.AuthToken,
			}

			resp, err := node.ApiClient.SyncStreams(ctx, req)
			if err != nil {
				s.Error("Failed to sync updated stream to node", "nodeID", node.ID, "error", err)
				return true
			}

			if resp.Code != 0 {
				s.Error("Node rejected stream update", "nodeID", node.ID, "message", resp.Message, "code", resp.Code)
				return true
			}

			s.Info("Successfully synced updated stream to node",
				"nodeID", node.ID,
				"streamPath", stream.StreamPath,
			)
		}

		return true
	})
}

// OnStreamAdded 处理流添加事件
func (s *StreamSyncService) OnStreamAdded(stream *StreamInfo) {
	// 只有管理节点需要同步到其他节点
	if s.plugin.Role == "manager" {
		// 如果启用了 etcd，使用 etcd 同步
		if s.plugin.Etcd.Enabled && s.plugin.etcdStreamSync != nil {
			// etcd 同步已经在 RegisterStreamInternal 中处理
			s.Debug("Using etcd for stream sync, skipping direct sync")
		} else {
			// 否则使用直接同步
			s.SyncStreamAddedToAllNodes(stream)
		}
	}
}

// OnStreamRemoved 处理流删除事件
func (s *StreamSyncService) OnStreamRemoved(streamPath string) {
	// 只有管理节点需要同步到其他节点
	if s.plugin.Role == "manager" {
		// 如果启用了 etcd，使用 etcd 同步
		if s.plugin.Etcd.Enabled && s.plugin.etcdStreamSync != nil {
			// etcd 同步已经在 UnregisterStreamInternal 中处理
			s.Debug("Using etcd for stream sync, skipping direct sync")
		} else {
			// 否则使用直接同步
			s.SyncStreamRemovedToAllNodes(streamPath)
		}
	}
}

// OnStreamUpdated 处理流更新事件
func (s *StreamSyncService) OnStreamUpdated(stream *StreamInfo) {
	// 只有管理节点需要同步到其他节点
	if s.plugin.Role == "manager" {
		// 如果启用了 etcd，使用 etcd 同步
		if s.plugin.Etcd.Enabled && s.plugin.etcdStreamSync != nil {
			// 更新 etcd 中的流信息
			if err := s.plugin.etcdStreamSync.UpdateStream(stream); err != nil {
				s.Error("Failed to update stream in etcd", "error", err)
			}
		} else {
			// 否则使用直接同步
			s.SyncStreamUpdatedToAllNodes(stream)
		}
	}
}

// HandleSyncStreamsRequest 处理同步流请求
func (s *StreamSyncService) HandleSyncStreamsRequest(req *pb.SyncStreamsRequest) (*pb.SyncStreamsResponse, error) {
	s.Info("Handling sync streams request",
		"fromNodeID", req.NodeId,
		"streamCount", len(req.Streams),
		"fullSync", req.FullSync,
	)

	// 验证授权令牌
	if req.AuthToken != s.plugin.AuthToken {
		return &pb.SyncStreamsResponse{
			Code:    401,
			Message: "授权失败：无效的令牌",
			Data:    &pb.SyncStreamsResponseData{},
		}, nil
	}

	// 如果是全量同步，先清空现有流信息
	if req.FullSync {
		// 只清除来自管理节点的流信息，保留本地发布的流
		s.plugin.streams.Range(func(stream *StreamInfo) bool {
			// 如果流不是本地发布的，则删除
			if stream.PublisherNodeID != s.plugin.NodeID {
				s.plugin.streams.RemoveByKey(stream.StreamPath)
			}
			return true
		})
	}

	// 处理流信息
	acceptedCount := 0
	for _, protoStream := range req.Streams {
		// 转换为内部格式
		streamInfo := convertProtoStreamInfoToInternal(protoStream)
		if streamInfo == nil {
			s.Error("Failed to convert proto stream info", "streamPath", protoStream.StreamPath)
			continue
		}

		// 检查是否与本地发布的流冲突
		existingStream, exists := s.plugin.streams.Get(streamInfo.StreamPath)
		if exists && existingStream.PublisherNodeID == s.plugin.NodeID {
			s.Warn("Ignoring synced stream that conflicts with local stream",
				"streamPath", streamInfo.StreamPath,
				"localPublisher", s.plugin.NodeID,
				"remotePublisher", streamInfo.PublisherNodeID,
			)
			continue
		}

		// 确保流信息有一个有效的上下文
		if streamInfo.Context == nil {
			// 如果没有上下文，创建一个新的背景上下文
			streamInfo.Context = context.Background()
		}

		// 添加或更新流信息
		s.plugin.streams.Add(streamInfo, s.plugin.Logger)
		acceptedCount++
	}

	s.Info("Sync streams request processed",
		"acceptedCount", acceptedCount,
		"totalReceived", len(req.Streams),
	)

	return &pb.SyncStreamsResponse{
		Code:    0,
		Message: "同步成功",
		Data: &pb.SyncStreamsResponseData{
			AcceptedCount: int32(acceptedCount),
			Timestamp:     time.Now().UnixMilli(),
		},
	}, nil
}

// HandleRemoveStreamRequest 处理删除流请求
func (s *StreamSyncService) HandleRemoveStreamRequest(req *pb.RemoveStreamRequest) (*pb.RemoveStreamResponse, error) {
	s.Info("Handling remove stream request",
		"fromNodeID", req.NodeId,
		"streamPath", req.StreamPath,
	)

	// 验证授权令牌
	if req.AuthToken != s.plugin.AuthToken {
		return &pb.RemoveStreamResponse{
			Code:    401,
			Message: "授权失败：无效的令牌",
			Data:    &pb.RemoveStreamResponseData{Timestamp: time.Now().UnixMilli()},
		}, nil
	}

	// 获取流信息
	streamInfo, exists := s.plugin.streams.Get(req.StreamPath)
	if !exists {
		return &pb.RemoveStreamResponse{
			Code:    0,
			Message: "流不存在，无需删除",
			Data:    &pb.RemoveStreamResponseData{Timestamp: time.Now().UnixMilli()},
		}, nil
	}

	// 检查是否是本地发布的流
	if streamInfo.PublisherNodeID == s.plugin.NodeID {
		s.Warn("Ignoring remove request for local stream",
			"streamPath", req.StreamPath,
			"localPublisher", s.plugin.NodeID,
		)
		return &pb.RemoveStreamResponse{
			Code:    403,
			Message: "无法删除本地发布的流",
			Data:    &pb.RemoveStreamResponseData{Timestamp: time.Now().UnixMilli()},
		}, nil
	}

	// 删除流信息
	s.plugin.streams.RemoveByKey(req.StreamPath)

	s.Info("Stream removed successfully", "streamPath", req.StreamPath)

	return &pb.RemoveStreamResponse{
		Code:    0,
		Message: "流删除成功",
		Data:    &pb.RemoveStreamResponseData{Removed: true, Timestamp: time.Now().UnixMilli()},
	}, nil
}

// FullSyncTask 定期全量同步任务
type FullSyncTask struct {
	task.TickTask
	service *StreamSyncService
}

// GetTickInterval 获取同步间隔
func (t *FullSyncTask) GetTickInterval() time.Duration {
	return t.service.plugin.Sync.FullSyncInterval
}

// Tick 执行同步
func (t *FullSyncTask) Tick(any) {
	// 只有管理节点执行全量同步
	if t.service.plugin.Role == "manager" {
		t.service.SyncStreamsToAllNodes()
	}
}
