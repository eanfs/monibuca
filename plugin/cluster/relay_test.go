//go:build cluster

package plugin_cluster

import (
	"encoding/json"
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

// TestRelay_OnSubscribe_CreatesPullProxyWithClusterRelayMarker 是 Phase 3
// 的核心场景: 远端有流(KV: live/foo = node-other),本节点订阅 → OnSubscribe →
// 检测出 remote → 通过 relayHook 触发 pull-proxy 创建。最后验证 hook 收到的
// conf 里:
//   - StreamPath = "live/foo"
//   - Type = "rtmp"(因 Advertise.RTMP 不空且优先级最高)
//   - URL  = "rtmp://10.0.0.1:1935/live/foo"
//   - originId = origin nodeID
//
// 注: cluster-relay 标记(Description 前缀)在 Task 10 收窄到 *PullProxyConfig
// 后才能从 map 里拿,本 Task 验证 4 个核心字段,Task 10 时再补 Description 断言。
func TestRelay_OnSubscribe_CreatesPullProxyWithClusterRelayMarker(t *testing.T) {
	client, addr := requireConsul(t)
	nodeID := uniqNodeID(t)
	streamPath := "live/" + nodeID + "-remote"
	p := startMembershipForTest(t, nodeID, addr)
	_ = startStreamRegistryForTest(t, p)

	// startMembershipForTest 绕开框架直接构造 ClusterPlugin,default tag 不生效,
	// 手动补上 relayProtocols 默认值确保 buildPullURL 能选到 rtmp。
	p.RelayProtocols = []string{"rtmp", "rtsp", "flv"}

	// 注入一个伪 peer。这里只用 KV 注 peer 信息,membership.Peer(originID) 通过 watcher 看到。
	originID := nodeID + "-origin"
	peerInfo := PeerInfo{
		NodeID:    originID,
		Advertise: AdvertiseConfig{RTMP: "10.0.0.1:1935"},
		StartedAt: time.Now().Unix(),
	}
	peerJSON, _ := json.Marshal(peerInfo)
	if _, err := client.KV().Put(&consulapi.KVPair{
		Key: keyNode(originID), Value: peerJSON,
	}, nil); err != nil {
		t.Fatalf("kv put node: %v", err)
	}
	// 等 nodeWatcher 把它收进 peers map。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := p.membership.Peer(originID); ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := p.membership.Peer(originID); !ok {
		t.Fatalf("nodeWatcher never saw injected peer within 2s")
	}

	// 注入 streamPath 的 owner = originID。
	if _, err := client.KV().Put(&consulapi.KVPair{
		Key: keyStream(streamPath), Value: []byte(originID),
	}, nil); err != nil {
		t.Fatalf("kv put stream: %v", err)
	}
	for time.Now().Before(deadline) {
		if owner, ok := p.streamRegistry.Lookup(streamPath); ok && owner == originID {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// recorder hook。
	var captured map[string]string
	p.relayHook = func(conf any) (bool, error) {
		if m, ok := conf.(map[string]string); ok {
			captured = m
		}
		return true, nil
	}

	p.OnSubscribe(streamPath, nil)

	if captured == nil {
		t.Fatalf("relayHook never called")
	}
	if captured["streamPath"] != streamPath {
		t.Errorf("captured streamPath = %q, want %q", captured["streamPath"], streamPath)
	}
	if captured["type"] != "rtmp" {
		t.Errorf("captured type = %q, want rtmp", captured["type"])
	}
	wantURL := "rtmp://10.0.0.1:1935/" + streamPath
	if captured["url"] != wantURL {
		t.Errorf("captured url = %q, want %q", captured["url"], wantURL)
	}
	if captured["originId"] != originID {
		t.Errorf("captured originId = %q, want %q", captured["originId"], originID)
	}
}
