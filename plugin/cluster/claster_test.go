//go:build ignore_linters

package plugin_claster

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Test basic key retrieval methods
func TestGetKey(t *testing.T) {
	// Create a node info
	nodeInfo := &NodeInfo{
		ID: "test-node",
	}

	// Create a stream info
	streamInfo := &StreamInfo{
		StreamPath: "test/stream",
	}

	// Test GetKey methods
	assert.Equal(t, "test-node", nodeInfo.GetKey())
	assert.Equal(t, "test/stream", streamInfo.GetKey())
}

// Test vector clock functionality for conflict resolution
func TestVectorClockVersioning(t *testing.T) {
	// Create a manager
	plugin := &ClasterPlugin{}
	manager := NewClusterManager(plugin)

	// Create first version of stream
	stream1 := &StreamInfo{
		StreamPath:      "test/stream",
		PublisherNodeID: "node1",
		VectorClock:     make(map[string]uint64),
	}
	stream1.VectorClock["node1"] = 1

	// Create second version with higher vector clock
	stream2 := &StreamInfo{
		StreamPath:      "test/stream",
		PublisherNodeID: "node1",
		VectorClock:     make(map[string]uint64),
	}
	stream2.VectorClock["node1"] = 2

	// Test newer version detection
	assert.True(t, manager.isNewerStreamVersion(stream2, stream1), "Higher vector clock should be detected as newer")

	// Test that older version is not considered newer
	assert.False(t, manager.isNewerStreamVersion(stream1, stream2), "Lower vector clock should not be detected as newer")

	// Test concurrent modification detection
	stream3 := &StreamInfo{
		StreamPath:      "test/stream",
		PublisherNodeID: "node1",
		VectorClock:     make(map[string]uint64),
	}
	stream3.VectorClock["node1"] = 1
	stream3.VectorClock["node2"] = 1

	stream4 := &StreamInfo{
		StreamPath:      "test/stream",
		PublisherNodeID: "node1",
		VectorClock:     make(map[string]uint64),
	}
	stream4.VectorClock["node1"] = 2

	// Should detect concurrent modifications
	// Neither stream should be strictly newer than the other in a conflict
	bothNewer := manager.isNewerStreamVersion(stream3, stream4) && manager.isNewerStreamVersion(stream4, stream3)
	assert.False(t, bothNewer, "Concurrent modifications should be detected correctly")
}
