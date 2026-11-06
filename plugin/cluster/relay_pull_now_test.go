//go:build cluster

package plugin_cluster

import (
	"encoding/json"
	"testing"
	"time"

	consulapi "github.com/hashicorp/consul/api"
)

// TestRelayPullOnlyOnCreate 钉住"新建 relay proxy 后必须立刻拉流一次"的调用点：
// created=true 触发一次、created=false 不触发。断言落在 pullRelayProxy 的注入点上，
// 删掉 ensureRelay 里的 pullNow 分支本用例就会红（只测判据函数做不到这一点）。
//
// 背景（2026-11-06 三机真机复现）：relay proxy 创建后状态是 Offline，而
// Server.OnSubscribe 只对 Status==Online 的 proxy 触发 Pull()，于是首次订阅者
// 必然等到 subscribe timeout（10s），第二次订阅才拿到流。
func TestRelayPullOnlyOnCreate(t *testing.T) {
	client, addr := requireConsul(t)
	nodeID := uniqNodeID(t)
	p := startMembershipForTest(t, nodeID, addr)
	_ = startStreamRegistryForTest(t, p)
	p.RelayProtocols = []string{"rtmp", "rtsp", "flv"}

	originID := nodeID + "-origin"
	peerJSON, _ := json.Marshal(PeerInfo{NodeID: originID, Advertise: AdvertiseConfig{RTMP: "10.0.0.1:1935"}})
	if _, err := client.KV().Put(&consulapi.KVPair{Key: keyNode(originID), Value: peerJSON}, nil); err != nil {
		t.Fatalf("kv put node: %v", err)
	}

	var pulled []string
	p.relayPullHook = func(streamPath string) { pulled = append(pulled, streamPath) }

	waitRelayLookup := func(streamPath string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, ok := p.membership.Peer(originID); !ok {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			if owner, ok := p.streamRegistry.Lookup(streamPath); !ok || owner != originID {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			return
		}
		t.Fatalf("relay lookup never saw %s -> %s", streamPath, originID)
	}

	// created=true：必须触发一次首发拉流。
	newStream := "live/" + nodeID + "-new"
	if _, err := client.KV().Put(&consulapi.KVPair{Key: keyStream(newStream), Value: []byte(originID)}, nil); err != nil {
		t.Fatalf("kv put stream: %v", err)
	}
	waitRelayLookup(newStream)
	p.relayHook = func(conf any) (bool, error) { return true, nil }
	p.OnSubscribe(newStream, nil)
	// created=false：proxy 已存在，不得重复 Pull（会打断正在运行的 PullJob）。
	reusedStream := "live/" + nodeID + "-reused"
	if _, err := client.KV().Put(&consulapi.KVPair{Key: keyStream(reusedStream), Value: []byte(originID)}, nil); err != nil {
		t.Fatalf("kv put stream: %v", err)
	}
	waitRelayLookup(reusedStream)
	p.relayHook = func(conf any) (bool, error) { return false, nil }
	p.OnSubscribe(reusedStream, nil)

	if len(pulled) != 1 || pulled[0] != newStream {
		t.Fatalf("expected exactly one pull for newly created relay proxy (%s), got %v", newStream, pulled)
	}
}

// TestRelayPullProxySkipsNilProxy 保证注入路径没有真实 proxy 时不会 panic：
// relayHook 只登记不建 proxy，pullRelayProxy 必须安静跳过。
func TestRelayPullProxySkipsNilProxy(t *testing.T) {
	p := &ClusterPlugin{}
	p.pullRelayProxy(nil, "live/nil-proxy") // 不应 panic
}
