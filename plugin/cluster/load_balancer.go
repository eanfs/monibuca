package plugin_claster

import (
	"fmt"
	"math"
	"sort"
	"time"

	"m7s.live/v5/pkg/task"
)

// LoadBalancerImpl handles load balancing decisions
type LoadBalancerImpl struct {
	task.TickTask
	plugin *ClusterPlugin

	// 节点评分缓存
	nodeScores     map[string]float64
	lastScoreTime  map[string]time.Time
	scoreCacheTTL  time.Duration
	scoreThreshold float64 // 节点评分阈值，低于此值的节点不参与负载均衡
}

// NewLoadBalancer creates a new LoadBalancer
func NewLoadBalancer(plugin *ClusterPlugin) ILoadBalancer {
	return &LoadBalancerImpl{
		plugin:         plugin,
		nodeScores:     make(map[string]float64),
		lastScoreTime:  make(map[string]time.Time),
		scoreCacheTTL:  time.Second * 5, // 评分缓存 5 秒
		scoreThreshold: 0.3,             // 评分阈值 0.3
	}
}

// Start initializes the LoadBalancer
func (lb *LoadBalancerImpl) Start() error {
	lb.Info("Starting load balancer")
	return nil
}

// GetTickInterval returns the interval for the ticker
func (lb *LoadBalancerImpl) GetTickInterval() time.Duration {
	return lb.plugin.LoadBalancing.CheckInterval
}

// Tick is called on each tick of the ticker
func (lb *LoadBalancerImpl) Tick(any) {
	lb.checkLoadBalance()
}

// checkLoadBalance checks if load balancing is needed
func (lb *LoadBalancerImpl) checkLoadBalance() {

	// 更新所有节点的评分
	lb.plugin.nodes.Range(func(node *NodeInfo) bool {
		// 如果评分缓存未过期，不重新计算
		if lastTime, ok := lb.lastScoreTime[node.ID]; ok {
			if time.Since(lastTime) < lb.scoreCacheTTL {
				return true
			}
		}

		// 重新计算节点评分
		score := lb.calculateNodeScore(node)
		lb.nodeScores[node.ID] = score
		lb.lastScoreTime[node.ID] = time.Now()
		return true
	})
}

// calculateNodeScore 计算节点评分
func (lb *LoadBalancerImpl) calculateNodeScore(node *NodeInfo) float64 {
	if node == nil {
		return 0
	}

	// 基础权重
	weights := lb.plugin.LoadBalancing.Weights

	// 计算 CPU 评分 (CPU 使用率越低越好)
	cpuScore := 1.0
	if node.Capacity.MaxCPUPercent > 0 {
		cpuUsage := node.CurrentLoad.CPUPercent / node.Capacity.MaxCPUPercent
		cpuScore = math.Max(0, 1-cpuUsage)
	}

	// 计算内存评分 (内存使用率越低越好)
	memoryScore := 1.0
	if node.Capacity.MaxMemoryGB > 0 {
		memoryUsage := node.CurrentLoad.MemoryGB / node.Capacity.MaxMemoryGB
		memoryScore = math.Max(0, 1-memoryUsage)
	}

	// 计算带宽评分 (带宽使用率越低越好)
	bandwidthScore := 1.0
	if node.Capacity.MaxBandwidthMbps > 0 {
		bandwidthUsage := float64(node.CurrentLoad.BandwidthMbps) / float64(node.Capacity.MaxBandwidthMbps)
		bandwidthScore = math.Max(0, 1-bandwidthUsage)
	}

	// 计算流数量评分 (流数量越少越好)
	streamsScore := 1.0
	if node.Capacity.MaxConcurrentStreams > 0 {
		streamsUsage := float64(node.CurrentLoad.ConcurrentStreams) / float64(node.Capacity.MaxConcurrentStreams)
		streamsScore = math.Max(0, 1-streamsUsage)
	}

	// 计算网络延迟评分 (延迟越低越好)
	latencyScore := 1.0
	if len(node.CurrentLoad.NetworkLatencyMs) > 0 {
		var totalLatency float64
		for _, latency := range node.CurrentLoad.NetworkLatencyMs {
			totalLatency += latency
		}
		avgLatency := totalLatency / float64(len(node.CurrentLoad.NetworkLatencyMs))
		// 假设 200ms 为最大可接受延迟
		latencyScore = math.Max(0, 1-avgLatency/200)
	}

	// 计算总评分
	totalScore := cpuScore*weights.CPU +
		memoryScore*weights.Memory +
		bandwidthScore*weights.Network +
		streamsScore*weights.Streams +
		latencyScore*0.1 // 网络延迟使用固定权重

	return totalScore
}

// GetNodeScore 获取节点评分
func (lb *LoadBalancerImpl) GetNodeScore(nodeID string) float64 {

	if score, ok := lb.nodeScores[nodeID]; ok {
		return score
	}
	return 0
}

// GetAllNodeScores 获取所有节点评分
func (lb *LoadBalancerImpl) GetAllNodeScores() map[string]float64 {

	scores := make(map[string]float64)
	for nodeID, score := range lb.nodeScores {
		scores[nodeID] = score
	}
	return scores
}

// UpdateScoreThreshold 更新评分阈值
func (lb *LoadBalancerImpl) UpdateScoreThreshold(threshold float64) {

	if threshold >= 0 && threshold <= 1 {
		lb.scoreThreshold = threshold
	}
}

// UpdateScoreCacheTTL 更新评分缓存时间
func (lb *LoadBalancerImpl) UpdateScoreCacheTTL(ttl time.Duration) {

	if ttl > 0 {
		lb.scoreCacheTTL = ttl
	}
}

// GetOptimalNodeForPublisher returns the optimal node for a new publisher
func (lb *LoadBalancerImpl) GetOptimalNodeForPublisher() (*NodeInfo, error) {

	// Get all available nodes
	var candidates []*NodeInfo

	lb.plugin.nodes.Range(func(node *NodeInfo) bool {
		// Skip nodes that are not healthy
		if node.Status != "healthy" || !node.Online {
			return true
		}

		// Skip nodes that don't accept publishers
		if node.Role != "edge" && node.Role != "worker" && node.Role != "manager" {
			return true
		}

		// 检查节点评分是否达到阈值
		if score, ok := lb.nodeScores[node.ID]; ok && score >= lb.scoreThreshold {
			candidates = append(candidates, node)
		}
		return true
	})

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no available nodes for publishing")
	}

	// 根据评分排序候选节点
	sort.Slice(candidates, func(i, j int) bool {
		scoreI := lb.nodeScores[candidates[i].ID]
		scoreJ := lb.nodeScores[candidates[j].ID]
		return scoreI > scoreJ
	})

	// 返回评分最高的节点
	return candidates[0], nil
}

// GetOptimalNodeForSubscriber returns the optimal node for a new subscriber
func (lb *LoadBalancerImpl) GetOptimalNodeForSubscriber(streamPath string) (*NodeInfo, error) {

	// First, check if the stream exists
	streamInfo, exists := lb.plugin.streams.Get(streamPath)
	if !exists {
		return nil, fmt.Errorf("stream not found: %s", streamPath)
	}

	// Try to find the publisher node first - it's the most optimal for subscribing
	publisherNode, exists := lb.plugin.nodes.Get(streamInfo.PublisherNodeID)
	if exists && publisherNode.Status == "healthy" && publisherNode.Online {
		// 检查发布者节点的评分
		if score, ok := lb.nodeScores[publisherNode.ID]; ok && score >= lb.scoreThreshold {
			return publisherNode, nil
		}
	}

	// If publisher node is not available, find other nodes that have this stream
	var candidates []*NodeInfo

	// Check all nodes that might have this stream
	for _, nodeID := range streamInfo.ReplicatedTo {
		node, exists := lb.plugin.nodes.Get(nodeID)
		if !exists || node.Status != "healthy" || !node.Online {
			continue
		}

		// 检查节点评分是否达到阈值
		if score, ok := lb.nodeScores[node.ID]; ok && score >= lb.scoreThreshold {
			candidates = append(candidates, node)
		}
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no available nodes for stream %s", streamPath)
	}

	// 根据评分排序候选节点
	sort.Slice(candidates, func(i, j int) bool {
		scoreI := lb.nodeScores[candidates[i].ID]
		scoreJ := lb.nodeScores[candidates[j].ID]
		return scoreI > scoreJ
	})

	// 返回评分最高的节点
	return candidates[0], nil
}

// OnNodeAdded is called when a node is added to the cluster
func (lb *LoadBalancerImpl) OnNodeAdded(nodeInfo *NodeInfo) {

	// 计算新节点的初始评分
	score := lb.calculateNodeScore(nodeInfo)
	lb.nodeScores[nodeInfo.ID] = score
	lb.lastScoreTime[nodeInfo.ID] = time.Now()
}

// OnNodeRemoved is called when a node is removed from the cluster
func (lb *LoadBalancerImpl) OnNodeRemoved(nodeInfo *NodeInfo) {

	delete(lb.nodeScores, nodeInfo.ID)
	delete(lb.lastScoreTime, nodeInfo.ID)
}

// OnStreamAdded is called when a stream is added to the cluster
func (lb *LoadBalancerImpl) OnStreamAdded(streamInfo *StreamInfo) {
	// 当流被添加时，更新相关节点的评分
	if streamInfo.PublisherNodeID != "" {
		if node, exists := lb.plugin.nodes.Get(streamInfo.PublisherNodeID); exists {
			lb.OnNodeUpdated(node)
		}
	}
	for _, nodeID := range streamInfo.ReplicatedTo {
		if node, exists := lb.plugin.nodes.Get(nodeID); exists {
			lb.OnNodeUpdated(node)
		}
	}
}

// OnStreamRemoved is called when a stream is removed from the cluster
func (lb *LoadBalancerImpl) OnStreamRemoved(streamInfo *StreamInfo) {
	// 当流被移除时，更新相关节点的评分
	if streamInfo.PublisherNodeID != "" {
		if node, exists := lb.plugin.nodes.Get(streamInfo.PublisherNodeID); exists {
			lb.OnNodeUpdated(node)
		}
	}
	for _, nodeID := range streamInfo.ReplicatedTo {
		if node, exists := lb.plugin.nodes.Get(nodeID); exists {
			lb.OnNodeUpdated(node)
		}
	}
}

// OnNodeUpdated is called when a node's state is updated
func (lb *LoadBalancerImpl) OnNodeUpdated(node *NodeInfo) {

	// 重新计算节点评分
	score := lb.calculateNodeScore(node)
	lb.nodeScores[node.ID] = score
	lb.lastScoreTime[node.ID] = time.Now()
}

// OnNodeStatusChanged is called when a node's status changes
func (lb *LoadBalancerImpl) OnNodeStatusChanged(node *NodeInfo) {
	lb.OnNodeUpdated(node)
}

// Stop implements the Stop method
func (lb *LoadBalancerImpl) Stop(err error) {
	lb.Info("Stopping load balancer")
}
