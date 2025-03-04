package plugin_claster

import (
	"time"

	"m7s.live/v5/pkg/task"
)

// ILoadBalancer defines the interface for load balancing operations
type ILoadBalancer interface {
	task.ITask
	GetOptimalNodeForPublisher() (*NodeInfo, error)
	GetOptimalNodeForSubscriber(streamPath string) (*NodeInfo, error)
	OnNodeAdded(nodeInfo *NodeInfo)
	OnNodeRemoved(nodeInfo *NodeInfo)
	OnStreamAdded(streamInfo *StreamInfo)
	OnStreamRemoved(streamInfo *StreamInfo)
	OnNodeStatusChanged(node *NodeInfo)
	OnNodeUpdated(node *NodeInfo)
	GetNodeScore(nodeID string) float64
	GetAllNodeScores() map[string]float64
	UpdateScoreThreshold(threshold float64)
	UpdateScoreCacheTTL(ttl time.Duration)
}

// IServiceDiscovery defines the interface for service discovery operations
type IServiceDiscovery interface {
	task.ITask
}
