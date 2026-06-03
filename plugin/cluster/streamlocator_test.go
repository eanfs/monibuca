//go:build cluster

package plugin_cluster

import (
	"encoding/json"
	"testing"
	"time"

	consulapi "github.com/hashicorp/consul/api"
)

// TestRedirectAdvisor_LocalStreamReturnsNoRedirect: streamPath 的 owner 是
// self → ok=false(本地流不需要 redirect)。
func TestRedirectAdvisor_LocalStreamReturnsNoRedirect(t *testing.T) {
	client, addr := requireConsul(t)
	nodeID := uniqNodeID(t)
	streamPath := "live/" + nodeID + "-local"
	p := startMembershipForTest(t, nodeID, addr)
	_ = startStreamRegistryForTest(t, p)

	_, _ = client.KV().Put(&consulapi.KVPair{Key: keyStream(streamPath), Value: []byte(nodeID)}, nil)
	waitForLookup(t, p, streamPath, nodeID)

	_, _, ok := p.GetRedirectTargetV2("flv", streamPath, "localhost", "http")
	if ok {
		t.Fatalf("expected ok=false for local stream")
	}
}

// TestRedirectAdvisor_RemoteStreamReturnsPeerHTTP: streamPath 的 owner 是远端
// + protocol 是 flv(默认 HTTP 端口)→ 返回 peer.Advertise.FLV 的 host:port。
func TestRedirectAdvisor_RemoteStreamReturnsPeerHTTP(t *testing.T) {
	client, addr := requireConsul(t)
	nodeID := uniqNodeID(t)
	originID := nodeID + "-origin"
	streamPath := "live/" + nodeID + "-remote"
	p := startMembershipForTest(t, nodeID, addr)
	_ = startStreamRegistryForTest(t, p)

	peerJSON, _ := json.Marshal(PeerInfo{
		NodeID:    originID,
		Advertise: AdvertiseConfig{FLV: "http://10.0.0.1:8080"},
	})
	_, _ = client.KV().Put(&consulapi.KVPair{Key: keyNode(originID), Value: peerJSON}, nil)
	_, _ = client.KV().Put(&consulapi.KVPair{Key: keyStream(streamPath), Value: []byte(originID)}, nil)
	waitForPeer(t, p, originID)
	waitForLookup(t, p, streamPath, originID)

	target, code, ok := p.GetRedirectTargetV2("flv", streamPath, "localhost", "http")
	if !ok {
		t.Fatalf("expected ok=true for remote stream")
	}
	if code != 302 {
		t.Errorf("statusCode = %d, want 302", code)
	}
	if target != "10.0.0.1:8080" {
		t.Errorf("targetHost = %q, want 10.0.0.1:8080", target)
	}
}

// TestRedirectAdvisor_RemoteRTSPUsesAdvertiseRTSP: protocol=rtsp → 返回 peer
// Advertise.RTSP(裸 host:port,不带 scheme)。
func TestRedirectAdvisor_RemoteRTSPUsesAdvertiseRTSP(t *testing.T) {
	client, addr := requireConsul(t)
	nodeID := uniqNodeID(t)
	originID := nodeID + "-origin"
	streamPath := "live/" + nodeID + "-rtsp"
	p := startMembershipForTest(t, nodeID, addr)
	_ = startStreamRegistryForTest(t, p)

	peerJSON, _ := json.Marshal(PeerInfo{
		NodeID:    originID,
		Advertise: AdvertiseConfig{RTSP: "10.0.0.1:554"},
	})
	_, _ = client.KV().Put(&consulapi.KVPair{Key: keyNode(originID), Value: peerJSON}, nil)
	_, _ = client.KV().Put(&consulapi.KVPair{Key: keyStream(streamPath), Value: []byte(originID)}, nil)
	waitForPeer(t, p, originID)
	waitForLookup(t, p, streamPath, originID)

	target, _, ok := p.GetRedirectTargetV2("rtsp", streamPath, "localhost", "")
	if !ok {
		t.Fatalf("expected ok=true for rtsp")
	}
	if target != "10.0.0.1:554" {
		t.Errorf("targetHost = %q, want 10.0.0.1:554", target)
	}
}

// TestRedirectAdvisor_UnknownStreamReturnsNoRedirect: Lookup 失败 → ok=false,
// 让 m7s 默认逻辑回 404 / wait queue。
func TestRedirectAdvisor_UnknownStreamReturnsNoRedirect(t *testing.T) {
	_, addr := requireConsul(t)
	nodeID := uniqNodeID(t)
	p := startMembershipForTest(t, nodeID, addr)
	_ = startStreamRegistryForTest(t, p)

	_, _, ok := p.GetRedirectTargetV2("flv", "live/unknown", "localhost", "http")
	if ok {
		t.Fatalf("expected ok=false for unknown stream")
	}
}

// Helpers
func waitForLookup(t *testing.T, p *ClusterPlugin, streamPath, expectedOwner string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if owner, ok := p.streamRegistry.Lookup(streamPath); ok && owner == expectedOwner {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Lookup never returned %q=%q within 2s", streamPath, expectedOwner)
}

func waitForPeer(t *testing.T, p *ClusterPlugin, peerID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := p.membership.Peer(peerID); ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Peer never saw %q within 2s", peerID)
}
