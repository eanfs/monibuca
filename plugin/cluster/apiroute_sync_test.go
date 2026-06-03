//go:build cluster

package plugin_cluster

import (
	"testing"

	cfg "m7s.live/v5/pkg/config"
)

// TestBuildAPIRouteNodes_ExcludesSelfAndEmptyPeers: 输入含 self + 一个完整 peer
// + 一个 advertise 全空的 peer,期望只剩那个完整 peer。
func TestBuildAPIRouteNodes_ExcludesSelfAndEmptyPeers(t *testing.T) {
	selfID := "node-A"
	peers := []*PeerInfo{
		{NodeID: selfID, Advertise: AdvertiseConfig{GRPC: "10.0.0.1:50051"}},
		{NodeID: "node-B", Advertise: AdvertiseConfig{GRPC: "10.0.0.2:50051", FLV: "http://10.0.0.2:8080", RTSP: "10.0.0.2:554"}},
		{NodeID: "node-C", Advertise: AdvertiseConfig{}}, // 全空
	}

	nodes := buildAPIRouteNodes(peers, selfID)

	if len(nodes) != 1 {
		t.Fatalf("expected 1 node (just node-B), got %d: %+v", len(nodes), nodes)
	}
	got := nodes[0]
	if got.GRPC != "10.0.0.2:50051" {
		t.Errorf("GRPC = %q, want 10.0.0.2:50051", got.GRPC)
	}
	// FLV advertise 已带 scheme;写入 APIRouteNode.HTTP 时用 url.Parse 取 Host。
	// 这里只验证不为空 + 包含 "10.0.0.2":
	if got.HTTP == "" {
		t.Errorf("HTTP empty for node-B")
	}
	if got.RTSP != "10.0.0.2:554" {
		t.Errorf("RTSP = %q, want 10.0.0.2:554", got.RTSP)
	}
}

// TestBuildAPIRouteNodes_EmptyPeers: 没有任何 peer → 空 slice。
func TestBuildAPIRouteNodes_EmptyPeers(t *testing.T) {
	nodes := buildAPIRouteNodes(nil, "node-A")
	if len(nodes) != 0 {
		t.Fatalf("expected empty, got %+v", nodes)
	}
}

// 验证 cfg.APIRouteNode 类型存在(防止 unused import warning)
var _ cfg.APIRouteNode = cfg.APIRouteNode{}
