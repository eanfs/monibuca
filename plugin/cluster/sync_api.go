package plugin_claster

import (
	"context"
	"errors"
	"time"

	"m7s.live/v5/plugin/cluster/pb"
)

// Error definitions
var (
	ErrNodeNotFound           = errors.New("node not found")
	ErrNodeOffline            = errors.New("node is offline")
	ErrSyncRejected           = errors.New("sync rejected by node")
	ErrNodeClientNotAvailable = errors.New("node client not available")
)

// SyncStreams implements the SyncStreams RPC method
func (p *ClusterPlugin) SyncStreams(ctx context.Context, req *pb.SyncStreamsRequest) (*pb.SyncStreamsResponse, error) {
	p.Info("Received SyncStreams request",
		"fromNodeID", req.NodeId,
		"streamCount", len(req.Streams),
		"fullSync", req.FullSync,
	)

	// 使用流同步服务处理请求
	return p.streamSyncService.HandleSyncStreamsRequest(req)
}

// RemoveStream implements the RemoveStream RPC method
func (p *ClusterPlugin) RemoveStream(ctx context.Context, req *pb.RemoveStreamRequest) (*pb.RemoveStreamResponse, error) {
	p.Info("Received RemoveStream request",
		"fromNodeID", req.NodeId,
		"streamPath", req.StreamPath,
	)

	// 使用流同步服务处理请求
	return p.streamSyncService.HandleRemoveStreamRequest(req)
}

// RequestFullSync implements the RequestFullSync RPC method
func (p *ClusterPlugin) RequestFullSync(ctx context.Context, req *pb.RequestFullSyncRequest) (*pb.RequestFullSyncResponse, error) {
	p.Info("Received RequestFullSync request", "fromNodeID", req.NodeId)

	// 验证授权令牌
	if req.AuthToken != p.AuthToken {
		return &pb.RequestFullSyncResponse{
			Code:    401,
			Message: "授权失败：无效的令牌",
			Data:    &pb.RequestFullSyncResponseData{},
		}, nil
	}

	// 检查是否是管理节点
	if p.Role != "manager" {
		return &pb.RequestFullSyncResponse{
			Code:    403,
			Message: "只有管理节点可以处理全量同步请求",
			Data:    &pb.RequestFullSyncResponseData{},
		}, nil
	}

	// 获取请求节点信息
	_, exists := p.nodes.Get(req.NodeId)
	if !exists {
		return &pb.RequestFullSyncResponse{
			Code:    404,
			Message: "节点不存在",
			Data:    &pb.RequestFullSyncResponseData{},
		}, nil
	}

	// 异步执行同步操作
	go p.streamSyncService.SyncStreamsToNode(req.NodeId)

	return &pb.RequestFullSyncResponse{
		Code:    0,
		Message: "同步请求已接受，正在处理",
		Data:    &pb.RequestFullSyncResponseData{Timestamp: time.Now().UnixMilli()},
	}, nil
}

// 更新 RegisterStreamInternal 方法，添加流同步
func (p *ClusterPlugin) RegisterStreamInternalWithSync(streamInfo *StreamInfo) error {
	// 创建一个背景上下文作为默认上下文
	backgroundCtx := context.Background()

	// 确保流信息有一个有效的上下文
	if streamInfo.Context == nil {
		// 如果没有上下文，使用背景上下文
		streamInfo.Context = backgroundCtx
	}

	// 先注册流
	err := p.RegisterStreamInternal(streamInfo)
	if err != nil {
		return err
	}

	// 如果是管理节点，同步到其他节点
	if p.Role == "manager" && p.streamSyncService != nil {
		p.streamSyncService.OnStreamAdded(streamInfo)
	}

	return nil
}

// 更新 UnregisterStreamInternal 方法，添加流同步
func (p *ClusterPlugin) UnregisterStreamInternalWithSync(streamPath string) error {
	// 获取流信息（用于同步）
	_, exists := p.streams.Get(streamPath)

	// 先注销流
	err := p.UnregisterStreamInternal(streamPath)
	if err != nil {
		return err
	}

	// 如果是管理节点，同步到其他节点
	if p.Role == "manager" && p.streamSyncService != nil && exists {
		p.streamSyncService.OnStreamRemoved(streamPath)
	}

	return nil
}
