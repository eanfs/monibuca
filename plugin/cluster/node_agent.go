package plugin_claster

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/mem"
	"m7s.live/v5/pkg/task"
)

// NodeAgent is responsible for collecting and reporting metrics, and managing local resources
type NodeAgent struct {
	task.Job
	plugin   *ClusterPlugin
	nodeInfo *NodeInfo
}

// HeartbeatTask handles periodic heartbeat messages
type HeartbeatTask struct {
	task.TickTask
	agent *NodeAgent
}

// MetricsTask handles periodic metrics collection
type MetricsTask struct {
	task.TickTask
	agent *NodeAgent
}

// GetTickInterval returns the heartbeat interval
func (t *HeartbeatTask) GetTickInterval() time.Duration {
	return t.agent.plugin.HeartbeatInterval
}

// GetTickInterval returns the metrics collection interval
func (t *MetricsTask) GetTickInterval() time.Duration {
	return t.agent.plugin.Monitoring.MetricsInterval
}

// Tick sends a heartbeat message
func (t *HeartbeatTask) Tick(any) {
	t.agent.sendHeartbeat()
}

// Tick collects system and stream metrics
func (t *MetricsTask) Tick(any) {
	t.agent.collectMetrics()
}

// NewNodeAgent creates a new NodeAgent
func NewNodeAgent(plugin *ClusterPlugin) *NodeAgent {
	tcp := plugin.GetGlobalCommonConf().TCP.ListenAddr
	port, err := strconv.Atoi(strings.Split(tcp, ":")[1])
	if err != nil {
		plugin.Error("Failed to convert TCP port to int", "error", err)
		port = 0
	}
	ip := strings.Split(tcp, ":")[0]
	if ip == "" {
		ip = plugin.GetPublicIP("")
	}

	totalMemoryGB := getSystemMemoryGB()
	agent := &NodeAgent{
		plugin: plugin,
		nodeInfo: &NodeInfo{
			ID:            plugin.NodeID,
			IP:            ip,
			Port:          port,
			Role:          plugin.Role,
			Region:        plugin.Region,
			Version:       "1.0.0", // TODO: Get actual version
			Status:        "initializing",
			JoinTime:      time.Now(),
			LastHeartbeat: time.Now(),
			Streams:       make(map[string]StreamInfo),
			StreamCount:   0,
			TotalMemoryGB: totalMemoryGB,
			Capacity: ResourceCapacity{
				MaxConcurrentStreams: plugin.Resources.MaxStreams,
				MaxBandwidthMbps:     plugin.Resources.MaxBandwidthMbps,
				MaxCPUPercent:        100 - plugin.Resources.ReserveCPUPercent,
				MaxMemoryGB:          float64(totalMemoryGB) * (1 - plugin.Resources.ReserveMemoryPercent/100),
				TranscodingCapacity:  calculateTranscodingCapacity(),
				ReserveCPUPercent:    plugin.Resources.ReserveCPUPercent,
				ReserveMemoryGB:      float64(totalMemoryGB) * plugin.Resources.ReserveMemoryPercent / 100,
			},
			CurrentLoad: ResourceUsage{
				NetworkLatencyMs: make(map[string]float64),
			},
			LocalNode: isLocalNode(plugin),
			Context:   context.Background(),
		},
	}

	// Add the node to the collection
	plugin.nodes.Set(agent.nodeInfo)

	return agent
}

// Start initializes the NodeAgent
func (a *NodeAgent) Start() error {
	a.Info("Starting node agent", "role", a.plugin.Role, "nodeID", a.nodeInfo.ID, "localNode", a.nodeInfo.LocalNode, "etcdEnabled", a.plugin.Etcd.Enabled)

	// Initialize heartbeat and metrics tasks
	a.AddTask(&HeartbeatTask{agent: a})
	a.AddTask(&MetricsTask{agent: a})

	// Set node status
	a.nodeInfo.Status = "healthy"
	a.nodeInfo.Online = true
	return nil
}

// Dispose cleans up resources
func (a *NodeAgent) Dispose() {
	a.Info("Stopping node agent", "nodeID", a.nodeInfo.ID)
}

// collectMetrics collects system and stream metrics
func (a *NodeAgent) collectMetrics() {
	// Get system resource usage from Server.Summary
	summary, err := a.plugin.Server.Summary(context.Background(), nil)
	if err == nil {
		a.nodeInfo.CurrentLoad.CPUPercent = float64(summary.CpuUsage)
		a.nodeInfo.CurrentLoad.MemoryGB = float64(summary.Memory.Used) / 1024 // Convert MB to GB
	}

	// Count current streams
	localStreamCount := 0
	totalBandwidth := 0.0

	// Iterate through all streams
	a.plugin.streams.Range(func(stream *StreamInfo) bool {
		if stream.PublisherNodeID == a.nodeInfo.ID {
			localStreamCount++
			totalBandwidth += stream.MediaInfo.BandwidthMbps
		}
		return true
	})

	a.nodeInfo.CurrentLoad.ConcurrentStreams = localStreamCount
	a.nodeInfo.StreamCount = localStreamCount
	a.nodeInfo.CurrentLoad.BandwidthMbps = int(totalBandwidth)

	// Update last heartbeat time
	a.nodeInfo.LastHeartbeat = time.Now()
}

// sendHeartbeat sends a heartbeat to the manager
func (a *NodeAgent) sendHeartbeat() {
	// Update heartbeat time
	a.nodeInfo.LastHeartbeat = time.Now()
}

// Helper functions

// generateNodeID generates a unique ID for this node
func generateNodeID(plugin *ClusterPlugin) string {
	// TODO: Implement a more robust ID generation
	return fmt.Sprintf("%s-%s-%d", plugin.Role, plugin.Region, time.Now().UnixNano())
}

// getSystemMemoryGB returns the total system memory in GB
func getSystemMemoryGB() float64 {
	v, err := mem.VirtualMemory()
	if err != nil {
		return 0
	}
	return float64(v.Total) / (1024 * 1024 * 1024)
}

// calculateTranscodingCapacity estimates the transcoding capacity of this node
func calculateTranscodingCapacity() int {
	// TODO: Implement a more accurate calculation
	return runtime.NumCPU() * 2
}

// isLocalNode checks if the node is running on the same machine as the manager
func isLocalNode(plugin *ClusterPlugin) bool {
	return plugin.Role == "manager"
}
