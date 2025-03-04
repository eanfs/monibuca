package plugin_claster

import (
	"context"
	"fmt"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/protobuf/encoding/protojson"
	"m7s.live/v5/plugin/cluster/pb"
)

// customMarshaler is a custom JSON marshaler that sets proper headers
var customMarshaler = &runtime.HTTPBodyMarshaler{
	Marshaler: &runtime.JSONPb{
		MarshalOptions: protojson.MarshalOptions{
			UseProtoNames:   true,
			EmitUnpopulated: true,
		},
	},
}

// GetClusterStatus 实现获取集群状态接口
func (p *ClusterPlugin) GetClusterStatus(ctx context.Context, req *pb.GetClusterStatusRequest) (*pb.GetClusterStatusResponse, error) {
	// 计算节点总数
	var totalNodes, healthyNodes int32
	p.nodes.Range(func(node *NodeInfo) bool {
		totalNodes++
		if node.Online && node.Status == "healthy" {
			healthyNodes++
		}
		return true
	})

	// 准备响应数据
	statusData := &pb.GetClusterStatusResponseData{
		Status: &pb.ClusterStatus{
			TotalNodes:   totalNodes,
			HealthyNodes: healthyNodes,
			ClusterState: p.ClusterState,
		},
	}

	// 创建响应
	response := &pb.GetClusterStatusResponse{
		Code:    0,
		Message: "获取集群状态成功",
		Data:    statusData,
	}

	// 统计节点状态
	var offlineNodes, unhealthyNodes int32
	var totalCPUPercent, totalMemoryGB, totalBandwidthMbps float64
	var totalConcurrentStreams int32

	p.nodes.Range(func(node *NodeInfo) bool {
		if !node.Online {
			offlineNodes++
		} else if node.Status != "healthy" {
			unhealthyNodes++
		}

		// 累计资源使用情况
		if node.Online && node.Status == "healthy" {
			totalCPUPercent += node.CurrentLoad.CPUPercent
			totalMemoryGB += node.CurrentLoad.MemoryGB
			totalBandwidthMbps += float64(node.CurrentLoad.BandwidthMbps)
			totalConcurrentStreams += int32(node.CurrentLoad.ConcurrentStreams)
		}

		return true
	})

	// 填充节点状态统计
	statusData.Status.DegradedNodes = unhealthyNodes
	statusData.Status.OfflineNodes = offlineNodes

	// 填充资源使用情况
	if statusData.Status.TotalNodes > 0 {
		statusData.Status.AverageCpuUsage = totalCPUPercent / float64(statusData.Status.TotalNodes)
		statusData.Status.AverageMemoryUsage = totalMemoryGB / float64(statusData.Status.TotalNodes)
		statusData.Status.TotalBandwidthMbps = totalBandwidthMbps
		statusData.Status.TotalStreams = int32(totalConcurrentStreams)
	}

	// 统计流状态
	var activeStreams, inactiveStreams int32
	p.streams.Range(func(stream *StreamInfo) bool {
		if stream.State == "active" {
			activeStreams++
		} else {
			inactiveStreams++
		}
		return true
	})

	// 填充流状态统计
	statusData.Status.ActiveStreams = activeStreams
	statusData.Status.TotalStreams = activeStreams + inactiveStreams

	// 填充节点分布信息
	statusData.Status.NodesByRegion = make(map[string]int32)
	statusData.Status.NodesByRole = make(map[string]int32)
	for role, count := range p.RoleCounts {
		statusData.Status.NodesByRole[role] = int32(count)
	}

	// 填充管理节点列表
	var managerNodes []string
	p.nodes.Range(func(node *NodeInfo) bool {
		if node.Role == "manager" && node.Status == "healthy" {
			managerNodes = append(managerNodes, node.ID)
		}
		return true
	})
	statusData.Status.ManagerNodes = managerNodes

	// 计算集群运行时间
	statusData.Status.UptimeSeconds = int64(time.Since(p.StartTime).Seconds())

	return response, nil
}

// GetNodes 实现获取节点列表接口
func (p *ClusterPlugin) GetNodes(ctx context.Context, req *pb.GetNodesRequest) (*pb.GetNodesResponse, error) {
	// 准备响应数据
	responseData := &pb.GetNodesResponseData{
		Nodes: make([]*pb.NodeInfo, 0),
	}

	// 创建响应
	response := &pb.GetNodesResponse{
		Code:    0,
		Message: "获取节点列表成功",
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

// HealthCheck 实现健康检查接口
func (p *ClusterPlugin) HealthCheck(ctx context.Context, req *pb.HealthCheckRequest) (*pb.HealthCheckResponse, error) {
	// 准备响应数据
	responseData := &pb.HealthCheckResponseData{
		Results: make([]*pb.HealthCheckResult, 0),
	}

	// 创建响应
	response := &pb.HealthCheckResponse{
		Code:    0,
		Message: "健康检查成功",
		Data:    responseData,
	}

	// 如果指定了节点ID，检查该节点的健康状态
	if req.TargetId != "" {
		nodeInfo, exists := p.nodes.Get(req.TargetId)
		if !exists {
			return &pb.HealthCheckResponse{
				Code:    404,
				Message: fmt.Sprintf("节点不存在: %s", req.TargetId),
				Data:    &pb.HealthCheckResponseData{},
			}, nil
		}

		if !nodeInfo.Online {
			return &pb.HealthCheckResponse{
				Code:    503,
				Message: fmt.Sprintf("节点离线: %s", req.TargetId),
				Data:    &pb.HealthCheckResponseData{},
			}, nil
		}

		if time.Since(nodeInfo.LastHeartbeat) > p.OfflineThreshold {
			return &pb.HealthCheckResponse{
				Code:    503,
				Message: fmt.Sprintf("节点心跳超时: %s", req.TargetId),
				Data:    &pb.HealthCheckResponseData{},
			}, nil
		}

		if nodeInfo.Status != "healthy" {
			return &pb.HealthCheckResponse{
				Code:    503,
				Message: fmt.Sprintf("节点状态异常: %s (%s)", req.TargetId, nodeInfo.Status),
				Data:    &pb.HealthCheckResponseData{},
			}, nil
		}

		// 添加节点健康状态到响应中
		result := &pb.HealthCheckResult{
			CheckName: "node_health",
			TargetId:  nodeInfo.ID,
			Status:    nodeInfo.Status,
			Message:   "节点健康状态正常",
			Value:     1.0,
			Timestamp: nodeInfo.LastHeartbeat.UnixMilli(),
		}
		responseData.Results = append(responseData.Results, result)
	}

	return response, nil
}

// UpdateNodeConfig 实现更新节点配置接口
func (p *ClusterPlugin) UpdateNodeConfig(ctx context.Context, req *pb.UpdateNodeConfigRequest) (*pb.UpdateNodeConfigResponse, error) {
	// 验证授权令牌
	if req.AuthToken != p.AuthToken {
		return &pb.UpdateNodeConfigResponse{
			Code:    401,
			Message: "授权失败：无效的令牌",
			Data:    &pb.UpdateNodeConfigResponseData{},
		}, nil
	}

	// 获取节点信息
	nodeInfo, exists := p.nodes.Get(req.NodeId)
	if !exists {
		return &pb.UpdateNodeConfigResponse{
			Code:    404,
			Message: "节点不存在: " + req.NodeId,
			Data:    &pb.UpdateNodeConfigResponseData{},
		}, nil
	}

	// 更新节点配置
	if req.Config != nil {
		// 更新资源配置
		if req.Config.Resources != nil {
			nodeInfo.Capacity.MaxConcurrentStreams = int(req.Config.Resources.MaxStreams)
			nodeInfo.Capacity.MaxBandwidthMbps = int(req.Config.Resources.MaxBandwidthMbps)
		}

		// 更新角色和区域
		if req.Config.Role != "" {
			nodeInfo.Role = req.Config.Role
		}
		if req.Config.Region != "" {
			nodeInfo.Region = req.Config.Region
		}
	}

	// 重新设置节点信息到集合中
	p.nodes.Set(nodeInfo)

	return &pb.UpdateNodeConfigResponse{
		Code:    0,
		Message: "更新节点配置成功",
		Data:    &pb.UpdateNodeConfigResponseData{Success: true},
	}, nil
}

// GetClusterMetrics 实现获取集群指标接口
func (p *ClusterPlugin) GetClusterMetrics(ctx context.Context, req *pb.GetClusterMetricsRequest) (*pb.GetClusterMetricsResponse, error) {
	// 准备响应数据
	metricsData := &pb.GetClusterMetricsResponseData{
		Metrics: &pb.ClusterMetrics{
			NodeMetrics:   make([]*pb.NodeMetrics, 0),
			StreamMetrics: make([]*pb.StreamMetrics, 0),
		},
	}

	// 创建响应
	response := &pb.GetClusterMetricsResponse{
		Code:    0,
		Message: "获取集群指标成功",
		Data:    metricsData,
	}

	// 收集节点指标
	p.nodes.Range(func(node *NodeInfo) bool {
		metrics := &pb.NodeMetrics{
			NodeId:        node.ID,
			Role:          node.Role,
			Region:        node.Region,
			Status:        node.Status,
			Online:        node.Online,
			LastHeartbeat: node.LastHeartbeat.UnixMilli(),
			ResourceUsage: convertInternalResourceUsageToProto(&node.CurrentLoad),
			Capacity:      convertInternalResourceCapacityToProto(&node.Capacity),
			StreamCount:   int32(node.StreamCount),
		}
		metricsData.Metrics.NodeMetrics = append(metricsData.Metrics.NodeMetrics, metrics)
		return true
	})

	// 收集流指标
	p.streams.Range(func(stream *StreamInfo) bool {
		metrics := &pb.StreamMetrics{
			StreamPath:      stream.StreamPath,
			PublisherNodeId: stream.PublisherNodeID,
			State:           stream.State,
			CreationTime:    stream.CreationTime.UnixMilli(),
			LastUpdated:     stream.LastUpdated.UnixMilli(),
			ReplicatedTo:    stream.ReplicatedTo,
			Subscribers:     int32(stream.SubscriberCount),
		}
		metricsData.Metrics.StreamMetrics = append(metricsData.Metrics.StreamMetrics, metrics)
		return true
	})

	return response, nil
}

// GetClusterConfig 实现获取集群配置接口
func (p *ClusterPlugin) GetClusterConfig(ctx context.Context, req *pb.GetClusterConfigRequest) (*pb.GetClusterConfigResponse, error) {
	// 准备响应数据
	configData := &pb.GetClusterConfigResponseData{
		Config: &pb.ClusterConfig{
			LoadBalancing: &pb.LoadBalancingConfig{
				Strategy:        p.LoadBalancing.Strategy,
				CheckIntervalMs: int32(p.LoadBalancing.CheckInterval.Milliseconds()),
				Weights: &pb.LoadBalancingWeights{
					Cpu:         p.LoadBalancing.Weights.CPU,
					Memory:      p.LoadBalancing.Weights.Memory,
					Network:     p.LoadBalancing.Weights.Network,
					StreamCount: p.LoadBalancing.Weights.Streams,
				},
			},
			HighAvailability: &pb.HighAvailabilityConfig{
				FailoverTimeoutMs:   int32(p.FailoverDelay.Milliseconds()),
				HeartbeatIntervalMs: int32(p.HeartbeatInterval.Milliseconds()),
			},
			Sync: &pb.SyncConfig{
				GossipIntervalMs:   int32(p.SyncInterval.Milliseconds()),
				FullSyncIntervalMs: int32(p.SyncInterval.Milliseconds()),
			},
			Monitoring: &pb.MonitoringConfig{
				MetricsIntervalMs: int32(p.SyncInterval.Milliseconds()),
				AlertThresholds: &pb.AlertThresholds{
					CpuPercent:    p.Monitoring.AlertThresholds.CPUPercent,
					MemoryPercent: p.Monitoring.AlertThresholds.MemoryPercent,
				},
			},
		},
	}

	// 创建响应
	response := &pb.GetClusterConfigResponse{
		Code:    0,
		Message: "获取集群配置成功",
		Data:    configData,
	}

	return response, nil
}

// UpdateClusterConfig 实现更新集群配置接口
func (p *ClusterPlugin) UpdateClusterConfig(ctx context.Context, req *pb.UpdateClusterConfigRequest) (*pb.UpdateClusterConfigResponse, error) {
	if req.Config == nil {
		return &pb.UpdateClusterConfigResponse{
			Code:    400,
			Message: "配置不能为空",
			Data:    &pb.UpdateClusterConfigResponseData{},
		}, nil
	}

	if req.Config.LoadBalancing != nil {
		if req.Config.LoadBalancing.Strategy != "" {
			p.LoadBalancing.Strategy = req.Config.LoadBalancing.Strategy
		}
		if req.Config.LoadBalancing.CheckIntervalMs > 0 {
			p.LoadBalancing.CheckInterval = time.Duration(req.Config.LoadBalancing.CheckIntervalMs) * time.Millisecond
		}
		if req.Config.LoadBalancing.Weights != nil {
			p.LoadBalancing.Weights.CPU = req.Config.LoadBalancing.Weights.Cpu
			p.LoadBalancing.Weights.Memory = req.Config.LoadBalancing.Weights.Memory
			p.LoadBalancing.Weights.Network = req.Config.LoadBalancing.Weights.Network
			p.LoadBalancing.Weights.Streams = req.Config.LoadBalancing.Weights.StreamCount
		}
	}

	if req.Config.HighAvailability != nil {
		if req.Config.HighAvailability.FailoverTimeoutMs > 0 {
			p.FailoverDelay = time.Duration(req.Config.HighAvailability.FailoverTimeoutMs) * time.Millisecond
		}
		if req.Config.HighAvailability.HeartbeatIntervalMs > 0 {
			p.HeartbeatInterval = time.Duration(req.Config.HighAvailability.HeartbeatIntervalMs) * time.Millisecond
		}
	}

	if req.Config.Sync != nil {
		if req.Config.Sync.GossipIntervalMs > 0 {
			p.SyncInterval = time.Duration(req.Config.Sync.GossipIntervalMs) * time.Millisecond
		}
	}

	if req.Config.Monitoring != nil {
		if req.Config.Monitoring.AlertThresholds != nil {
			p.Monitoring.AlertThresholds.CPUPercent = req.Config.Monitoring.AlertThresholds.CpuPercent
			p.Monitoring.AlertThresholds.MemoryPercent = req.Config.Monitoring.AlertThresholds.MemoryPercent
		}
	}

	return &pb.UpdateClusterConfigResponse{
		Code:    0,
		Message: "集群配置更新成功",
		Data:    &pb.UpdateClusterConfigResponseData{Success: true},
	}, nil
}

// 将内部资源使用情况转换为 proto 格式
func convertInternalResourceUsageToProto(usage *ResourceUsage) *pb.ResourceUsage {
	return &pb.ResourceUsage{
		CpuPercent:        usage.CPUPercent,
		MemoryGb:          usage.MemoryGB,
		BandwidthMbps:     float64(usage.BandwidthMbps),
		ConcurrentStreams: int32(usage.ConcurrentStreams),
	}
}

// convertInternalResourceCapacityToProto 将内部 ResourceCapacity 转换为 proto ResourceCapacity
func convertInternalResourceCapacityToProto(capacity *ResourceCapacity) *pb.ResourceCapacity {
	if capacity == nil {
		return nil
	}

	pbCapacity := &pb.ResourceCapacity{
		MaxConcurrentStreams: int32(capacity.MaxConcurrentStreams),
		MaxBandwidthMbps:     float64(capacity.MaxBandwidthMbps),
		MaxCpuPercent:        capacity.MaxCPUPercent,
		MaxMemoryGb:          capacity.MaxMemoryGB,
		MaxTranscodingSlots:  int32(capacity.TranscodingCapacity),
	}
	return pbCapacity
}
