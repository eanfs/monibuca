//go:build cluster

package plugin_cluster

import (
	"testing"
	"time"

	consulapi "github.com/hashicorp/consul/api"
)

// ---------------------------------------------------------------------
// buildPullURL: 纯函数,根据 RelayProtocols 优先级 + peer 的 Advertise 表
// 选第一个可用协议,拼成完整 pull URL。不需 consul,纯单元测试。
// ---------------------------------------------------------------------

func TestRelay_BuildPullURL_PicksRTMPWhenAvailable(t *testing.T) {
	peer := &PeerInfo{
		NodeID: "node-a",
		Advertise: AdvertiseConfig{
			RTMP: "10.0.0.1:1935",
			RTSP: "10.0.0.1:554",
			FLV:  "http://10.0.0.1:8080",
		},
	}
	proto, fullURL, err := buildPullURL(peer, "live/foo", []string{"rtmp", "rtsp", "flv"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "rtmp" {
		t.Errorf("proto = %q, want rtmp", proto)
	}
	if fullURL != "rtmp://10.0.0.1:1935/live/foo" {
		t.Errorf("url = %q, want rtmp://10.0.0.1:1935/live/foo", fullURL)
	}
}

func TestRelay_BuildPullURL_FallsBackWhenFirstProtocolMissing(t *testing.T) {
	peer := &PeerInfo{
		NodeID: "node-a",
		Advertise: AdvertiseConfig{
			RTSP: "10.0.0.1:554",
		},
	}
	proto, fullURL, err := buildPullURL(peer, "live/foo", []string{"rtmp", "rtsp", "flv"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "rtsp" {
		t.Errorf("proto = %q, want rtsp", proto)
	}
	if fullURL != "rtsp://10.0.0.1:554/live/foo" {
		t.Errorf("url = %q, want rtsp://10.0.0.1:554/live/foo", fullURL)
	}
}

func TestRelay_BuildPullURL_FLVUsesAdvertisedSchemeAndAppendsDotFLV(t *testing.T) {
	peer := &PeerInfo{
		NodeID: "node-a",
		Advertise: AdvertiseConfig{
			FLV: "http://10.0.0.1:8080",
		},
	}
	proto, fullURL, err := buildPullURL(peer, "live/foo", []string{"rtmp", "rtsp", "flv"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "flv" {
		t.Errorf("proto = %q, want flv", proto)
	}
	if fullURL != "http://10.0.0.1:8080/live/foo.flv" {
		t.Errorf("url = %q, want http://10.0.0.1:8080/live/foo.flv", fullURL)
	}
}

func TestRelay_BuildPullURL_NoMatchingProtocolReturnsError(t *testing.T) {
	peer := &PeerInfo{Advertise: AdvertiseConfig{}}
	_, _, err := buildPullURL(peer, "live/foo", []string{"rtmp", "rtsp", "flv"})
	if err == nil {
		t.Fatalf("expected error when no protocol matches, got nil")
	}
}

// TestRelay_EnsureRelay_SkipsWhenStreamUnknown 验证: streamRegistry.Lookup
// 返回 false 时,ensureRelay 不应触发任何 EnsurePullProxy 调用(因为根本
// 不知道去哪拉)。
func TestRelay_EnsureRelay_SkipsWhenStreamUnknown(t *testing.T) {
	_, addr := requireConsul(t)
	nodeID := uniqNodeID(t)
	p := startMembershipForTest(t, nodeID, addr)
	_ = startStreamRegistryForTest(t, p)

	// recorder fake:任何 EnsurePullProxy 调用都被记录。
	var calls []string
	p.relayHook = func(conf any) (bool, error) {
		calls = append(calls, "called")
		return false, nil
	}

	p.OnSubscribe("live/unknown-stream", nil)

	if len(calls) != 0 {
		t.Fatalf("relayHook called %d times for unknown stream, want 0", len(calls))
	}
}

// TestRelay_EnsureRelay_SkipsWhenStreamLocal 验证: Lookup 返回的 owner ==
// 本节点 NodeID 时(本地流),ensureRelay 跳过。订阅本地流不需要 cluster relay。
func TestRelay_EnsureRelay_SkipsWhenStreamLocal(t *testing.T) {
	client, addr := requireConsul(t)
	nodeID := uniqNodeID(t)
	streamPath := "live/" + nodeID + "-local"
	p := startMembershipForTest(t, nodeID, addr)
	sr := startStreamRegistryForTest(t, p)

	// 直接 KV.Put 一个 owned-by-self 的流位置,等 watcher 看到。
	if _, err := client.KV().Put(&consulapi.KVPair{
		Key: keyStream(streamPath), Value: []byte(nodeID),
	}, nil); err != nil {
		t.Fatalf("kv put: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if owner, ok := sr.Lookup(streamPath); ok && owner == nodeID {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	var calls []string
	p.relayHook = func(conf any) (bool, error) {
		calls = append(calls, "called")
		return false, nil
	}

	p.OnSubscribe(streamPath, nil)

	if len(calls) != 0 {
		t.Fatalf("relayHook called %d times for local stream, want 0", len(calls))
	}
}
