package plugin_claster

import (
	"context"
	"fmt"
	"time"

	"m7s.live/v5/plugin/cluster/pb"
)

// SendHeartbeat 处理节点心跳
func (p *ClusterPlugin) SendHeartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	// 准备响应数据
	responseData := &pb.HeartbeatResponseData{
		ServerTime: time.Now().UnixMilli(),
	}

	// 创建响应
	response := &pb.HeartbeatResponse{
		Code:    0,
		Message: "Heartbeat received",
		Data:    responseData,
	}
	p.Info("Received Heartbeat", "req", req.NodeId)

	// 更新节点信息
	if node, exists := p.nodes.Get(req.NodeId); exists {
		// 更新资源使用情况
		if req.ResourceUsage != nil {
			node.CurrentLoad = ResourceUsage{
				CPUPercent:        req.ResourceUsage.CpuPercent,
				MemoryGB:          req.ResourceUsage.MemoryGb,
				BandwidthMbps:     int(req.ResourceUsage.BandwidthMbps),
				ConcurrentStreams: int(req.ResourceUsage.ConcurrentStreams),
				TranscodingLoad:   int(req.ResourceUsage.ActiveTranscodingTasks),
				NetworkLatencyMs:  req.LatencyMs,
			}
		}

		// 更新活跃流列表
		if len(req.ActiveStreams) > 0 {
			node.StreamCount = len(req.ActiveStreams)
		}

		// 更新流动态信息
		if len(req.StreamDynamicInfo) > 0 {
			p.updateStreamDynamicInfo(req.NodeId, req.StreamDynamicInfo)
		}

		// 更新心跳时间戳
		node.LastHeartbeat = time.Now()

		// 更新节点状态为在线
		node.Status = "healthy"
		node.Online = true

		// 检查是否需要全量同步
		responseData.NeedFullSync = p.needFullSync(node)
	}

	// 收集其他节点的更新信息
	responseData.NodeUpdates = make([]*pb.NodeUpdate, 0)
	p.nodes.Range(func(node *NodeInfo) bool {
		// 跳过当前节点
		if node.ID == req.NodeId {
			return true
		}

		// 只返回在线节点
		if !node.Online {
			return true
		}

		// 创建更新信息
		update := &pb.NodeUpdate{
			NodeId:      node.ID,
			StreamCount: int32(node.StreamCount),
		}

		// 添加资源使用情况
		if node.CurrentLoad.ConcurrentStreams > 0 {
			update.ResourceUsage = &pb.ResourceUsage{
				CpuPercent:             node.CurrentLoad.CPUPercent,
				MemoryGb:               node.CurrentLoad.MemoryGB,
				BandwidthMbps:          float64(node.CurrentLoad.BandwidthMbps),
				ConcurrentStreams:      int32(node.CurrentLoad.ConcurrentStreams),
				ActiveTranscodingTasks: int32(node.CurrentLoad.TranscodingLoad),
			}
		}

		// 添加网络延迟信息
		if len(node.CurrentLoad.NetworkLatencyMs) > 0 {
			update.LatencyMs = node.CurrentLoad.NetworkLatencyMs
		}

		responseData.NodeUpdates = append(responseData.NodeUpdates, update)
		return true
	})

	// 收集流更新信息
	responseData.StreamUpdates = p.collectStreamUpdates(req.NodeId)

	return response, nil
}

// GetNodeInfo 实现获取节点信息接口
func (p *ClusterPlugin) GetNodeInfo(ctx context.Context, req *pb.GetNodeInfoRequest) (*pb.GetNodeInfoResponse, error) {
	// 获取节点信息
	nodeInfo, exists := p.nodes.Get(req.NodeId)
	if !exists {
		return &pb.GetNodeInfoResponse{
			Code:    404,
			Message: "节点不存在: " + req.NodeId,
			Data:    &pb.GetNodeInfoResponseData{},
		}, nil
	}

	// 转换为 proto NodeInfo
	protoNodeInfo := convertInternalNodeInfoToProto(nodeInfo)

	return &pb.GetNodeInfoResponse{
		Code:    0,
		Message: "获取节点信息成功",
		Data:    &pb.GetNodeInfoResponseData{NodeInfo: protoNodeInfo},
	}, nil
}

// GetClusterNodes 实现获取集群节点列表接口
func (p *ClusterPlugin) GetClusterNodes(ctx context.Context, req *pb.GetClusterNodesRequest) (*pb.GetClusterNodesResponse, error) {
	// 准备响应数据
	responseData := &pb.GetClusterNodesResponseData{
		Nodes: make([]*pb.NodeInfo, 0),
	}

	// 创建响应
	response := &pb.GetClusterNodesResponse{
		Code:    0,
		Message: "获取集群节点列表成功",
		Data:    responseData,
	}

	// 收集所有节点信息
	p.nodes.Range(func(node *NodeInfo) bool {
		// 如果指定了区域过滤器，则只返回该区域的节点
		if req.RegionFilter != "" && node.Region != req.RegionFilter {
			return true // 继续遍历
		}

		// 如果指定了角色过滤器，则只返回该角色的节点
		if req.RoleFilter != "" && node.Role != req.RoleFilter {
			return true // 继续遍历
		}

		// 如果指定了状态过滤器，则只返回该状态的节点
		if req.StatusFilter != "" && node.Status != req.StatusFilter {
			return true // 继续遍历
		}

		// 转换为 proto NodeInfo 并添加到响应中
		protoNode := convertInternalNodeInfoToProto(node)
		if protoNode != nil {
			responseData.Nodes = append(responseData.Nodes, protoNode)
		}

		return true // 继续遍历
	})

	return response, nil
}

// UpdateNodeStatus 实现更新节点状态接口
func (p *ClusterPlugin) UpdateNodeStatus(ctx context.Context, req *pb.UpdateNodeStatusRequest) (*pb.UpdateNodeStatusResponse, error) {
	// 验证授权令牌
	if req.AuthToken != p.AuthToken {
		return &pb.UpdateNodeStatusResponse{
			Code:    401,
			Message: "授权失败：无效的令牌",
			Data:    &pb.UpdateNodeStatusResponseData{},
		}, nil
	}

	// 获取节点信息
	nodeInfo, exists := p.nodes.Get(req.NodeId)
	if !exists {
		return &pb.UpdateNodeStatusResponse{
			Code:    404,
			Message: "节点不存在: " + req.NodeId,
			Data:    &pb.UpdateNodeStatusResponseData{},
		}, nil
	}

	// 更新节点状态
	nodeInfo.Status = req.Status
	p.nodes.Set(nodeInfo)

	return &pb.UpdateNodeStatusResponse{
		Code:    0,
		Message: "节点状态更新成功",
		Data:    &pb.UpdateNodeStatusResponseData{Success: true},
	}, nil
}

// 辅助函数：将内部 NodeInfo 转换为 proto NodeInfo
func convertInternalNodeInfoToProto(node *NodeInfo) *pb.NodeInfo {
	if node == nil {
		return nil
	}

	// 构建地址字符串
	address := node.IP
	if node.Port > 0 {
		address = fmt.Sprintf("%s:%d", node.IP, node.Port)
	}

	protoNode := &pb.NodeInfo{
		Id:            node.ID,
		Address:       address,
		Role:          node.Role,
		Region:        node.Region,
		Status:        node.Status,
		LastHeartbeat: node.LastHeartbeat.UnixMilli(),
		StreamCount:   int32(node.StreamCount),
		Metadata:      node.Tags,
	}

	// 转换资源容量
	protoNode.Capacity = &pb.ResourceCapacity{
		MaxConcurrentStreams: int32(node.Capacity.MaxConcurrentStreams),
		MaxBandwidthMbps:     float64(node.Capacity.MaxBandwidthMbps),
		MaxCpuPercent:        node.Capacity.MaxCPUPercent,
		MaxMemoryGb:          node.Capacity.MaxMemoryGB,
		MaxTranscodingSlots:  int32(node.Capacity.TranscodingCapacity),
	}

	// 转换当前负载
	protoNode.CurrentLoad = &pb.ResourceUsage{
		ConcurrentStreams:      int32(node.CurrentLoad.ConcurrentStreams),
		BandwidthMbps:          float64(node.CurrentLoad.BandwidthMbps),
		CpuPercent:             node.CurrentLoad.CPUPercent,
		MemoryGb:               node.CurrentLoad.MemoryGB,
		ActiveTranscodingTasks: int32(node.CurrentLoad.TranscodingLoad),
	}

	return protoNode
}

// 辅助函数：检查是否需要全量同步
func (p *ClusterPlugin) needFullSync(node *NodeInfo) bool {
	// 如果节点状态为 offline，需要全量同步
	if node.Status == "offline" {
		return true
	}

	// 如果节点最后心跳时间超过健康检查间隔的 2 倍，需要全量同步
	if time.Since(node.LastHeartbeat) > p.HealthCheckInterval*2 {
		return true
	}

	return false
}

// updateStreamDynamicInfo 更新流动态信息
func (p *ClusterPlugin) updateStreamDynamicInfo(nodeID string, streamInfoMap map[string]*pb.StreamDynamicInfo) {
	// 遗历所有流动态信息
	for streamPath, dynamicInfo := range streamInfoMap {
		// 获取流信息
		stream, exists := p.streams.Get(streamPath)
		if !exists {
			// 如果流不存在，跳过
			p.Debug("Stream not found for dynamic info update", "streamPath", streamPath)
			continue
		}

		// 检查流是否属于该节点
		if stream.PublisherNodeID != nodeID {
			// 如果流不属于该节点，跳过
			p.Debug("Stream not published by this node", "streamPath", streamPath, "publisher", stream.PublisherNodeID, "nodeID", nodeID)
			continue
		}

		// 更新流动态信息
		stream.MediaInfo.BandwidthMbps = dynamicInfo.BandwidthMbps
		stream.MediaInfo.Framerate = dynamicInfo.Fps
		stream.SubscriberCount = int(dynamicInfo.SubscriberCount)

		// 只有当更新时间更新时才更新 LastUpdated
		updateTime := time.UnixMilli(dynamicInfo.LastUpdated)
		if updateTime.After(stream.LastUpdated) {
			stream.LastUpdated = updateTime
		}

		// 更新流信息
		p.streams.Add(stream)
		p.Debug("Updated stream dynamic info", "streamPath", streamPath)
	}
}

// collectStreamUpdates 收集流更新信息
func (p *ClusterPlugin) collectStreamUpdates(nodeID string) []*pb.StreamUpdate {
	// 创建流更新列表
	updates := make([]*pb.StreamUpdate, 0)

	// 遗历所有流
	p.streams.Range(func(stream *StreamInfo) bool {
		// 跳过当前节点发布的流
		if stream.PublisherNodeID == nodeID {
			return true
		}

		// 创建流动态信息
		dynamicInfo := &pb.StreamDynamicInfo{
			BandwidthMbps:   stream.MediaInfo.BandwidthMbps,
			Fps:             stream.MediaInfo.Framerate,
			SubscriberCount: int32(stream.SubscriberCount),
			LastUpdated:     stream.LastUpdated.UnixMilli(),
		}

		// 创建流更新
		update := &pb.StreamUpdate{
			StreamPath:  stream.StreamPath,
			DynamicInfo: dynamicInfo,
		}

		// 添加到更新列表
		updates = append(updates, update)
		return true
	})

	return updates
}
