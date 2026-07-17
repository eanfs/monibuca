//go:build cluster

package plugin_cluster

import (
	"errors"
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
	sr.handleLocalPublish("live/foo", true, nil, func(f func()) {
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
	sr.handleLocalPublish("", false, nil, func(f func()) {
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

// TestStreamRegistry_HandleLocalPublishStopsPublisherWhenKeyOwnedByPeer
// 验证 §4.3 first-write-wins 失败路径:外部已 KV.Acquire 了 m7s/streams/X(模拟
// 另一个节点 A 持有该流),本节点 B 尝试 acquire(同 X) 应当 KV.Acquire 返回 ok=false,
// 此时 handleLocalPublish 必须把 stopReason 投递到 onStopReason channel(测试用注入)。
//
// 注:生产路径下我们 pub.Stop(ErrStreamPathTaken),但测试直接用 *m7s.Publisher
// 太重(需要完整 Server)。这个测试用 onStopReason 注入点解耦。
func TestStreamRegistry_HandleLocalPublishStopsPublisherWhenKeyOwnedByPeer(t *testing.T) {
	client, addr := requireConsul(t)
	nodeID := uniqNodeID(t)
	streamPath := "live/" + nodeID + "-contested"
	p := startMembershipForTest(t, nodeID, addr)
	sr := startStreamRegistryForTest(t, p)

	// 用另一个独立 session 抢先占住 streamPath。
	otherSession, _, err := client.Session().Create(&consulapi.SessionEntry{
		Name:      "other-cluster-test",
		TTL:       "10s",
		Behavior:  consulapi.SessionBehaviorDelete,
		LockDelay: time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("create other session: %v", err)
	}
	t.Cleanup(func() { _, _ = client.Session().Destroy(otherSession, nil) })

	ok, _, err := client.KV().Acquire(&consulapi.KVPair{
		Key: keyStream(streamPath), Value: []byte("peer-node"), Session: otherSession,
	}, nil)
	if err != nil || !ok {
		t.Fatalf("seed acquire ok=%v err=%v", ok, err)
	}

	// 注入一个 stop 通道(新设计:停流回调由 OnPublish 直接传入 = pub.Stop,不再 SafeGet)。
	stopCh := make(chan error, 1)
	stop := func(reason error) { stopCh <- reason }

	// 模拟 OnPublish: 不是 cluster-relay,非空 streamPath,stop=spy,registerOnDispose=nil。
	// 真实路径下 acquire+停流被 dispatch 到 StreamRegistry 自身事件循环异步执行
	// (sr 已 start),所以这里 stop 会异步收到 ErrStreamPathTaken。
	sr.handleLocalPublish(streamPath, false, stop, nil)

	select {
	case got := <-stopCh:
		if !errors.Is(got, ErrStreamPathTaken) {
			t.Fatalf("stop reason = %v, want ErrStreamPathTaken", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("stop not fired within 2s")
	}
}

// TestClusterPlugin_IsActiveRelay 验证 RC3 修复的权威 relay 判据:ensureRelay 写入
// activeRelays 后,isActiveRelay 必须返回 true;未注册的返回 false。
func TestClusterPlugin_IsActiveRelay(t *testing.T) {
	p := &ClusterPlugin{}
	if p.isActiveRelay("live/x") {
		t.Fatal("empty activeRelays must report false")
	}
	p.activeRelaysMu.Lock()
	p.activeRelays = map[string]struct{}{"live/relayed": {}}
	p.activeRelaysMu.Unlock()
	if !p.isActiveRelay("live/relayed") {
		t.Fatal("registered active relay must report true")
	}
	if p.isActiveRelay("live/other") {
		t.Fatal("unregistered streamPath must report false")
	}
}

// TestStreamRegistry_ConflictHandlingOffloaded 是这次死锁修复的核心回归守卫(RC1+RC2)。
//
// 缺陷:OnPublish 在 Server.Streams 事件循环 goroutine 上同步跑 acquire(阻塞 Consul I/O)
// 和冲突停流,停流又重入 Server.Streams.SafeGet(m.Call)→ 永久死锁整个 Streams 事件循环。
//
// 契约:handleLocalPublish 必须把 acquire+冲突停流 **dispatch 出去异步执行**,
// 绝不在调用方 goroutine 上同步跑 acquire / stop。
//
// 纯单元测试,不依赖 consul / 事件循环:注入 dispatch 捕获被 offload 的闭包。
func TestStreamRegistry_ConflictHandlingOffloaded(t *testing.T) {
	sr := &StreamRegistry{localStreams: make(map[string]struct{})}

	acquireCalled := false
	sr.acquireFn = func(string) error {
		acquireCalled = true
		return errors.New("kv acquire returned false; held by peer")
	}
	var captured func()
	sr.dispatch = func(fn func()) { captured = fn } // 捕获,不同步执行

	stopCalled := false
	var stopReason error
	sr.handleLocalPublish("live/contested", false, func(r error) {
		stopCalled = true
		stopReason = r
	}, nil)

	// 关键断言:绝不能在调用方 goroutine 上同步跑 acquire / stop(否则就是会死锁的旧行为)。
	if acquireCalled {
		t.Fatal("acquire ran synchronously on caller goroutine — must be offloaded (RC1/RC2)")
	}
	if stopCalled {
		t.Fatal("stop ran synchronously on caller goroutine — re-entrancy deadlock risk (RC1)")
	}
	if captured == nil {
		t.Fatal("conflict handling was not dispatched off the caller goroutine")
	}

	// 执行被 offload 的闭包,验证它会 acquire 并在冲突时用 ErrStreamPathTaken 停流。
	captured()
	if !acquireCalled {
		t.Fatal("dispatched closure did not call acquire")
	}
	if !stopCalled || !errors.Is(stopReason, ErrStreamPathTaken) {
		t.Fatalf("dispatched closure must stop with ErrStreamPathTaken; stopCalled=%v reason=%v", stopCalled, stopReason)
	}
}

// TestStreamRegistry_ConflictDoesNotReleasePeerKey 守护一个次生 bug:
// acquire 冲突(键属于别的 peer)时,本节点 publisher 被停;其 dispose hook
// 绝不能调 release —— release 会 KV.Delete 这个键,把属主 peer 的所有权删掉。
func TestStreamRegistry_ConflictDoesNotReleasePeerKey(t *testing.T) {
	sr := &StreamRegistry{localStreams: make(map[string]struct{})}
	sr.acquireFn = func(string) error { return errors.New("held by peer") }
	releaseCalled := false
	sr.releaseFn = func(string) error { releaseCalled = true; return nil }
	sr.dispatch = func(fn func()) { fn() } // 同步执行便于断言

	var disposeHook func()
	sr.handleLocalPublish("live/x", false, func(error) {}, func(h func()) { disposeHook = h })

	if disposeHook == nil {
		t.Fatal("dispose hook should still be registered")
	}
	disposeHook() // 模拟 publisher dispose
	if releaseCalled {
		t.Fatal("must NOT release/delete KV key on conflict — it belongs to the peer node")
	}
}

// TestStreamRegistry_SuccessfulAcquireReleasesOnDispose 正向:成功 acquire 后,
// dispose 时必须 release 自己拥有的键。
func TestStreamRegistry_SuccessfulAcquireReleasesOnDispose(t *testing.T) {
	sr := &StreamRegistry{localStreams: make(map[string]struct{})}
	sr.acquireFn = func(string) error { return nil } // 成功
	releaseCalled := false
	sr.releaseFn = func(string) error { releaseCalled = true; return nil }
	sr.dispatch = func(fn func()) { fn() }

	var disposeHook func()
	sr.handleLocalPublish("live/x", false, func(error) {}, func(h func()) { disposeHook = h })

	disposeHook()
	if !releaseCalled {
		t.Fatal("must release acquired key on dispose")
	}
}

// TestStreamRegistry_DisposeBeforeAcquireCompletes 守护 TOCTOU 幽灵流:
// publisher 在异步 acquire 完成前就 dispose(秒断/Consul 慢)。dispose 时
// acquired=false 跳过 release;acquire 事后成功,必须回查 disposed 并补偿
// release —— 否则 KV 键被本节点健康 session 永久持有,流名全集群被毒化。
func TestStreamRegistry_DisposeBeforeAcquireCompletes(t *testing.T) {
	sr := &StreamRegistry{localStreams: make(map[string]struct{})}
	sr.acquireFn = func(string) error { return nil }
	releaseCount := 0
	sr.releaseFn = func(string) error { releaseCount++; return nil }
	var queue []func()
	sr.dispatch = func(fn func()) { queue = append(queue, fn) } // 捕获,模拟异步

	var disposeHook func()
	sr.handleLocalPublish("live/x", false, func(error) {}, func(h func()) { disposeHook = h })

	// acquire 已入队未执行时 publisher 先 dispose
	disposeHook()
	if releaseCount != 0 {
		t.Fatalf("release must not run before acquire succeeded, got %d", releaseCount)
	}
	// acquire 此后才完成 → 补偿 release 必须恰好执行一次
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		fn()
	}
	if releaseCount != 1 {
		t.Fatalf("compensating release must run exactly once, got %d", releaseCount)
	}
}

// TestStreamRegistry_DisposeReleaseIsDispatched 守护 Streams 循环阻塞(RC2 对称面):
// dispose 回调跑在 Server.Streams 事件循环上,release 是阻塞式 Consul I/O,
// 必须经 dispatch 挪走,绝不能在 dispose 调用栈里同步执行。
func TestStreamRegistry_DisposeReleaseIsDispatched(t *testing.T) {
	sr := &StreamRegistry{localStreams: make(map[string]struct{})}
	sr.acquireFn = func(string) error { return nil }
	releaseCalled := false
	sr.releaseFn = func(string) error { releaseCalled = true; return nil }
	var queue []func()
	sr.dispatch = func(fn func()) { queue = append(queue, fn) }

	var disposeHook func()
	sr.handleLocalPublish("live/x", false, func(error) {}, func(h func()) { disposeHook = h })
	// 先让 acquire 完成
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		fn()
	}

	disposeHook()
	if releaseCalled {
		t.Fatal("release must NOT run on dispose caller stack (Server.Streams 事件循环)")
	}
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		fn()
	}
	if !releaseCalled {
		t.Fatal("release should run via dispatched closure")
	}
}
