//go:build cluster

package plugin_cluster

import (
	"testing"
	"time"

	consulapi "github.com/hashicorp/consul/api"
)

// TestStreamRegistry_HandleLocalPublishSkipsClusterRelay 验证 Q2 决策的环回防护:
// cluster-relay 派生的 publisher(Description 带 "cluster-relay:" 前缀)绝不能写
// 流位置到 Consul,也绝不应该注册 dispose hook —— 否则 C→B→A→B 环。
//
// 纯单元测试,不需要 consul。
func TestStreamRegistry_HandleLocalPublishSkipsClusterRelay(t *testing.T) {
	sr := &StreamRegistry{localStreams: make(map[string]struct{})}
	sr.handleLocalPublish("live/foo", true, func(f func()) {
		t.Fatalf("registerOnDispose must not be called for cluster-relay publisher")
	})
	if len(sr.localStreams) != 0 {
		t.Fatalf("localStreams must remain empty for cluster-relay, got %v", sr.localStreams)
	}
}

// TestStreamRegistry_HandleLocalPublishSkipsEmptyStreamPath 防御性:streamPath
// 为空时(理论不该发生)整条路径必须 no-op,不能 panic、不能写 consul。
func TestStreamRegistry_HandleLocalPublishSkipsEmptyStreamPath(t *testing.T) {
	sr := &StreamRegistry{localStreams: make(map[string]struct{})}
	sr.handleLocalPublish("", false, func(f func()) {
		t.Fatalf("registerOnDispose must not be called for empty streamPath")
	})
	if len(sr.localStreams) != 0 {
		t.Fatalf("localStreams must remain empty for empty streamPath, got %v", sr.localStreams)
	}
}

// TestStreamRegistry_AcquireReleaseLifecycle 验证 acquire/release 与 consul KV 的契约:
//   - acquire 后 m7s/streams/<path> 存在,value=NodeID,session=当前 sid
//   - release 后键消失
func TestStreamRegistry_AcquireReleaseLifecycle(t *testing.T) {
	client, addr := requireConsul(t)
	nodeID := uniqNodeID(t)
	streamPath := "live/" + nodeID
	p := startMembershipForTest(t, nodeID, addr)
	sr := startStreamRegistryForTest(t, p)

	if err := sr.acquire(streamPath); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	pair, _, err := client.KV().Get(keyStream(streamPath), nil)
	if err != nil {
		t.Fatalf("kv get after acquire: %v", err)
	}
	if pair == nil {
		t.Fatalf("expected key %s to exist after acquire", keyStream(streamPath))
	}
	if string(pair.Value) != nodeID {
		t.Errorf("key value = %q, want %q", string(pair.Value), nodeID)
	}
	if pair.Session == "" {
		t.Errorf("key must be session-locked after acquire")
	}

	if err := sr.release(streamPath); err != nil {
		t.Fatalf("release: %v", err)
	}
	pair, _, err = client.KV().Get(keyStream(streamPath), nil)
	if err != nil {
		t.Fatalf("kv get after release: %v", err)
	}
	if pair != nil {
		t.Fatalf("expected key %s to be deleted after release, got %+v", keyStream(streamPath), pair)
	}
}

// TestStreamRegistry_WatcherReflectsRemoteWrite 验证 Phase 3 跨节点 relay 依赖
// 的 Lookup 路径:远端节点写流位置 → 本地 watcher 拉到 → Lookup 立即返回。
func TestStreamRegistry_WatcherReflectsRemoteWrite(t *testing.T) {
	client, addr := requireConsul(t)
	nodeID := uniqNodeID(t)
	streamPath := "live/" + nodeID + "-remote"
	p := startMembershipForTest(t, nodeID, addr)
	sr := startStreamRegistryForTest(t, p)

	if _, err := client.KV().Put(&consulapi.KVPair{
		Key:   keyStream(streamPath),
		Value: []byte("remote-node"),
	}, nil); err != nil {
		t.Fatalf("kv put: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if owner, ok := sr.Lookup(streamPath); ok && owner == "remote-node" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("watcher never reflected %s=remote-node within 2s", streamPath)
}

// TestStreamRegistry_AddOnStreamRemoved_FiresWhenKeyDeleted 验证 §4.2 触发条件:
// 当外部把 m7s/streams/<path> 键删了,streamWatcher 在下一轮 blocking query
// 中能感知到删除,并把消失的 streamPath 投递给所有 AddOnStreamRemoved 注册的回调。
func TestStreamRegistry_AddOnStreamRemoved_FiresWhenKeyDeleted(t *testing.T) {
	client, addr := requireConsul(t)
	nodeID := uniqNodeID(t)
	streamPath := "live/" + nodeID + "-watched"
	p := startMembershipForTest(t, nodeID, addr)
	sr := startStreamRegistryForTest(t, p)

	removedCh := make(chan string, 4)
	sr.AddOnStreamRemoved(func(sp string) { removedCh <- sp })

	// 写一个 key,等 watcher 看到。
	if _, err := client.KV().Put(&consulapi.KVPair{
		Key:   keyStream(streamPath),
		Value: []byte("remote-node"),
	}, nil); err != nil {
		t.Fatalf("kv put: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := sr.Lookup(streamPath); ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := sr.Lookup(streamPath); !ok {
		t.Fatalf("watcher never saw initial put within 2s")
	}

	// 删 key,期望 onStreamRemoved 被调用,且参数 = streamPath。
	if _, err := client.KV().Delete(keyStream(streamPath), nil); err != nil {
		t.Fatalf("kv delete: %v", err)
	}
	select {
	case got := <-removedCh:
		if got != streamPath {
			t.Errorf("onStreamRemoved got %q, want %q", got, streamPath)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("onStreamRemoved not fired within 3s after delete")
	}
}

// TestStreamRegistry_RebindAllReAcquiresLocalStreams 验证 A1 闭环:
// session 重建之后,所有本地 publishers 的流位置键必须被新 session 重新 Acquire。
// 直接调用 rebindAll 模拟"membership 通知重建"那一刻的行为。
func TestStreamRegistry_RebindAllReAcquiresLocalStreams(t *testing.T) {
	client, addr := requireConsul(t)
	nodeID := uniqNodeID(t)
	streamA := "live/" + nodeID + "-a"
	streamB := "live/" + nodeID + "-b"
	p := startMembershipForTest(t, nodeID, addr)
	sr := startStreamRegistryForTest(t, p)

	sr.localMu.Lock()
	sr.localStreams[streamA] = struct{}{}
	sr.localStreams[streamB] = struct{}{}
	sr.localMu.Unlock()

	sid := p.membership.SessionID()
	if sid == "" {
		t.Fatalf("session id is empty before rebind")
	}
	sr.rebindAll(sid)

	for _, sp := range []string{streamA, streamB} {
		pair, _, err := client.KV().Get(keyStream(sp), nil)
		if err != nil {
			t.Fatalf("kv get %s: %v", sp, err)
		}
		if pair == nil {
			t.Fatalf("rebindAll did not acquire key %s", sp)
		}
		if string(pair.Value) != nodeID {
			t.Errorf("key %s value = %q, want %q", sp, string(pair.Value), nodeID)
		}
		if pair.Session != sid {
			t.Errorf("key %s session = %q, want %q", sp, pair.Session, sid)
		}
	}
}
