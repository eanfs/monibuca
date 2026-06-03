//go:build cluster

package plugin_cluster

import (
	"encoding/json"
	"testing"
	"time"

	consulapi "github.com/hashicorp/consul/api"
)

// TestMembership_RegistersSelfAndPeer 启动 Membership 后:
//   - Consul KV 上 m7s/nodes/<self> 必须存在,且 JSON 解码后 NodeID/Advertise 正确
//   - 本地 watcher 最终能从 Peer(self) 看到自己
func TestMembership_RegistersSelfAndPeer(t *testing.T) {
	client, addr := requireConsul(t)
	nodeID := uniqNodeID(t)
	p := startMembershipForTest(t, nodeID, addr)

	pair, _, err := client.KV().Get(keyNode(nodeID), nil)
	if err != nil {
		t.Fatalf("kv get: %v", err)
	}
	if pair == nil {
		t.Fatalf("expected key %q to exist after Membership start", keyNode(nodeID))
	}
	var pi PeerInfo
	if err := json.Unmarshal(pair.Value, &pi); err != nil {
		t.Fatalf("unmarshal PeerInfo: %v", err)
	}
	if pi.NodeID != nodeID {
		t.Errorf("PeerInfo.NodeID = %q, want %q", pi.NodeID, nodeID)
	}
	if pi.Advertise.RTMP != "127.0.0.1:1935" {
		t.Errorf("PeerInfo.Advertise.RTMP = %q, want 127.0.0.1:1935", pi.Advertise.RTMP)
	}
	if pair.Session == "" {
		t.Errorf("KV pair must be session-locked")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if peer, ok := p.membership.Peer(nodeID); ok && peer.NodeID == nodeID {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Membership.Peer(self) never reflected the registered node within 2s")
}

// TestMembership_WatcherSeesRemotePeer 注入一个外部 peer key,验证本地 watcher
// 能在 WaitTime 周期内拉到并更新 peers map。
func TestMembership_WatcherSeesRemotePeer(t *testing.T) {
	client, addr := requireConsul(t)
	selfID := uniqNodeID(t)
	remoteID := selfID + "-remote"
	p := startMembershipForTest(t, selfID, addr)

	remote := PeerInfo{
		NodeID:    remoteID,
		Advertise: AdvertiseConfig{RTMP: "10.0.0.2:1935"},
		StartedAt: time.Now().Unix(),
	}
	b, err := json.Marshal(remote)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := client.KV().Put(&consulapi.KVPair{Key: keyNode(remoteID), Value: b}, nil); err != nil {
		t.Fatalf("kv put: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, peer := range p.membership.Peers() {
			if peer.NodeID == remoteID && peer.Advertise.RTMP == "10.0.0.2:1935" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("watcher never saw remote peer %s within 2s", remoteID)
}

// TestMembership_SessionDestroyTriggersRebuild 模拟 Phase 1 设计的 A1 触发条件:
// 外部把当前 session 删了,sessionTask 内部 retry 会重新建立 session,
// fireSessionRebuilt 携新 sid 触发。订阅方(Phase 2 StreamRegistry)依赖这个保证。
func TestMembership_SessionDestroyTriggersRebuild(t *testing.T) {
	client, addr := requireConsul(t)
	nodeID := uniqNodeID(t)
	p := startMembershipForTest(t, nodeID, addr)

	sid1 := p.membership.SessionID()
	if sid1 == "" {
		t.Fatalf("initial session id is empty")
	}

	// 注:首次 fireSessionRebuilt 在 startMembershipForTest 等待 SessionID 时已经触发,
	// 这里只关心 destroy 之后的"二次重建"事件。
	rebuiltCh := make(chan string, 4)
	p.membership.AddOnSessionRebuilt(func(sid string) { rebuiltCh <- sid })

	if _, err := client.Session().Destroy(sid1, nil); err != nil {
		t.Fatalf("destroy session: %v", err)
	}

	select {
	case got := <-rebuiltCh:
		if got == sid1 {
			t.Fatalf("rebuilt callback reported the OLD session id %q", got)
		}
		if got == "" {
			t.Fatalf("rebuilt callback reported empty session id")
		}
		if cur := p.membership.SessionID(); cur != got {
			t.Errorf("Membership.SessionID() = %q, but rebuilt callback reported %q", cur, got)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("session was not rebuilt within 5s after external destroy (current sid=%q, initial=%q)",
			p.membership.SessionID(), sid1)
	}
}
