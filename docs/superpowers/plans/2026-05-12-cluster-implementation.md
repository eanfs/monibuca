# Cluster v1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 Monibuca v5 的 cluster plugin 从 Phase 1+2(已 ready)推到 Phase 3-6 + 三节点 e2e 全跑通。

**Architecture:** 单一 Go 包 `plugin/cluster/`(build tag `cluster`),靠 Consul session lease 维持成员 / 流位置;Relay 在 OnSubscribe 时用 EnsurePullProxy 起跨节点拉流;StreamLocator 把现有 RouteInterceptor / RedirectAdvisorV2 接到 Consul 数据源;LoadReporter TickTask 周期写 metrics;e2e 用 docker-compose 跑 3 m7s + 1 consul + 1 pg + 1 minio。

**Tech Stack:** Go 1.24+, `github.com/hashicorp/consul/api`, `github.com/langhuihui/gotask`, m7s.live/v5 现有插件 + task 系统, docker + docker-compose, hashicorp/consul:latest, minio/minio:latest, postgres:16

**Spec:** `docs/superpowers/specs/2026-05-12-cluster-design.md`(必读;本 plan 不重复"为什么")

**Reference commits already on branch `feature/cluser2605`:**
- `6145fdcf` — Phase 1+2 tests-after + 3 production bug fixes(基线,本 plan 起点)
- 工作树有 4 个未 commit 的 Phase 3 文件(`relay.go`, `relay_test.go` + 测试还在 in-progress) —— **本 plan T0 把这些 stash 或 discard,回到干净基线再按 plan 走**

**Pre-flight:**
- `consul` docker 容器已在 `m7s-consul-test` 名字下跑;若没跑:`docker run --rm -d --name m7s-consul-test -p 8500:8500 hashicorp/consul:latest agent -dev -client=0.0.0.0`
- Go: `/opt/homebrew/bin/go`(1.26.3)
- 每个 Task 末尾的 commit 才算真完成

---

## Task 0: 工作树基线复位

**Files:**
- Modify: working tree(stash 当前未 commit 的 Phase 3 残件)

- [ ] **Step 1: 看现状**

```bash
git status
```
Expected: `plugin/cluster/relay.go`, `plugin/cluster/relay_test.go` 在 untracked,可能还有 `plugin/cluster/membership_test.go` 或别的微改

- [ ] **Step 2: stash 起来(不丢)**

```bash
git stash push -u -m "Phase 3 brainstorm-prior残件 — relay.go + relay_test.go buildPullURL 已 GREEN" plugin/cluster/relay.go plugin/cluster/relay_test.go
git status
```
Expected: 工作树清洁,残件躲在 stash@{0}

- [ ] **Step 3: 确认基线绿**

```bash
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/... -count=1 2>&1 | tail -3
```
Expected: `ok m7s.live/v5/plugin/cluster ...s`

---

## Task 1: Sentinel errors

**Files:**
- Create: `plugin/cluster/errors.go`

- [ ] **Step 1: 写 errors.go**

```go
//go:build cluster

package plugin_cluster

import "errors"

var (
	// ErrOriginLost 是 §4.2 中 Relay 主动 Stop 本节点 cluster-relay pull-proxy 时用的
	// reason。Lookup 探测到 streamPath 已不在任何节点上时,触发本 error。
	ErrOriginLost = errors.New("cluster: origin node lost")

	// ErrStreamPathTaken 是 §4.3 first-write-wins 失败时 publisher.Stop 用的 reason。
	// 本节点试图 acquire 一个 streamPath,但 KV 上已被另一个 session 持有。
	ErrStreamPathTaken = errors.New("cluster: streamPath already owned by peer")
)
```

- [ ] **Step 2: 编译验证**

```bash
/opt/homebrew/bin/go vet -tags cluster ./plugin/cluster/... 2>&1 | tail -3
```
Expected: 无输出 = vet 干净

- [ ] **Step 3: commit**

```bash
git add plugin/cluster/errors.go
git commit -m "feat(cluster): 加 sentinel errors ErrOriginLost / ErrStreamPathTaken

Phase 3 Relay 失去 origin 时主动 Stop pull-proxy 的 reason,
Phase 2 first-write-wins 失败时 publisher.Stop 的 reason。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: AddOnStreamRemoved 钩子 — RED

**Files:**
- Modify: `plugin/cluster/streamregistry.go`
- Test: `plugin/cluster/streamregistry_test.go`

- [ ] **Step 1: 写 RED 测试**

把这段加到 `plugin/cluster/streamregistry_test.go` 末尾:

```go
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
```

- [ ] **Step 2: 跑 RED**

```bash
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/ -run TestStreamRegistry_AddOnStreamRemoved -count=1 -v 2>&1 | tail -8
```
Expected: 编译失败 `sr.AddOnStreamRemoved undefined`

---

## Task 3: AddOnStreamRemoved 钩子 — GREEN

**Files:**
- Modify: `plugin/cluster/streamregistry.go`

- [ ] **Step 1: 在 `StreamRegistry` struct 加字段**

在 streamregistry.go 中,找到 `type StreamRegistry struct {`,在 `localStreams` 后加:

```go
	onStreamRemovedMu sync.Mutex
	onStreamRemoved   []func(streamPath string)
```

- [ ] **Step 2: 在 `Lookup` 方法下方,加 public 注册接口 + 内部 fire 函数**

```go
// AddOnStreamRemoved 注册一个回调,在 watcher 检测到 m7s/streams/<path> 键被删
// (因任何原因:session 失效 / 主动 release / 显式 Delete)时同步调用。
// Phase 3 Relay 用这个来在 origin 失联时立即 Stop 本节点的 cluster-relay pull-proxy。
func (sr *StreamRegistry) AddOnStreamRemoved(f func(streamPath string)) {
	sr.onStreamRemovedMu.Lock()
	defer sr.onStreamRemovedMu.Unlock()
	sr.onStreamRemoved = append(sr.onStreamRemoved, f)
}

func (sr *StreamRegistry) fireStreamRemoved(streamPath string) {
	sr.onStreamRemovedMu.Lock()
	cbs := append([]func(string){}, sr.onStreamRemoved...)
	sr.onStreamRemovedMu.Unlock()
	for _, cb := range cbs {
		cb(streamPath)
	}
}
```

- [ ] **Step 3: 修改 `streamWatcher.refresh` 使其 diff 新旧 streams 并 fire**

找到 `func (w *streamWatcher) refresh(pairs consulapi.KVPairs) {`,把 body 替换为:

```go
func (w *streamWatcher) refresh(pairs consulapi.KVPairs) {
	np := make(map[string]string, len(pairs))
	for _, p := range pairs {
		path := strings.TrimPrefix(p.Key, prefixStreams)
		if path == "" {
			continue
		}
		np[path] = string(p.Value)
	}

	// diff: 找上一次有、这次没了的 path,触发回调。
	w.sr.mu.RLock()
	removed := make([]string, 0)
	for old := range w.sr.streams {
		if _, still := np[old]; !still {
			removed = append(removed, old)
		}
	}
	w.sr.mu.RUnlock()

	w.sr.replace(np)

	for _, path := range removed {
		w.sr.fireStreamRemoved(path)
	}
}
```

- [ ] **Step 4: 跑 GREEN**

```bash
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/ -run TestStreamRegistry_AddOnStreamRemoved -count=1 -v 2>&1 | tail -5
```
Expected: `PASS`

- [ ] **Step 5: 全套回归**

```bash
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/... -count=1 2>&1 | tail -3
```
Expected: `ok`

- [ ] **Step 6: commit**

```bash
git add plugin/cluster/streamregistry.go plugin/cluster/streamregistry_test.go
git commit -m "feat(cluster): StreamRegistry 加 AddOnStreamRemoved 钩子

streamWatcher.refresh 现在 diff 新旧 streams,把消失的 streamPath
派发给所有注册回调。Phase 3 Relay 用这个在 origin 失联时立刻
Stop 本节点的 cluster-relay pull-proxy(§4.2)。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: First-write-wins (§4.3) — RED

**Files:**
- Test: `plugin/cluster/streamregistry_test.go`

- [ ] **Step 1: 写 RED 测试**

加到 `plugin/cluster/streamregistry_test.go` 末尾:

```go
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

	// 注入一个 onStopReason 通道。
	stopCh := make(chan error, 1)
	sr.SetOnStopPublisher(func(sp string, reason error) {
		if sp == streamPath {
			stopCh <- reason
		}
	})

	// 模拟 OnPublish: 不是 cluster-relay,非空 streamPath,registerOnDispose=nil。
	sr.handleLocalPublish(streamPath, false, nil)

	select {
	case got := <-stopCh:
		if !errors.Is(got, ErrStreamPathTaken) {
			t.Fatalf("stop reason = %v, want ErrStreamPathTaken", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("onStopPublisher not fired within 2s")
	}
}
```

注意:test 文件顶部 import 区可能需要 `"errors"`。如果之前没 import,加。

- [ ] **Step 2: 跑 RED**

```bash
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/ -run TestStreamRegistry_HandleLocalPublishStopsPublisher -count=1 -v 2>&1 | tail -8
```
Expected: 编译失败 `sr.SetOnStopPublisher undefined`

---

## Task 5: First-write-wins (§4.3) — GREEN

**Files:**
- Modify: `plugin/cluster/streamregistry.go`

- [ ] **Step 1: 在 `StreamRegistry` struct 加字段**

在 onStreamRemoved 字段下方加:

```go
	onStopPublisherMu sync.Mutex
	onStopPublisher   func(streamPath string, reason error)
```

- [ ] **Step 2: 加 public setter**

在 AddOnStreamRemoved 下方加:

```go
// SetOnStopPublisher 给 cluster 主插件设置一个回调:当 handleLocalPublish 发现
// streamPath 已被别的 peer 占据(§4.3 first-write-wins 失败),用这个回调主动
// Stop 本节点的 publisher 并报 ErrStreamPathTaken。
//
// 单一回调而非多 listener: 一个 streamPath 在一个进程里只对应一个 publisher,
// 也只有 cluster 主插件知道怎么 Stop 它(通过 m7s API)。
func (sr *StreamRegistry) SetOnStopPublisher(f func(streamPath string, reason error)) {
	sr.onStopPublisherMu.Lock()
	defer sr.onStopPublisherMu.Unlock()
	sr.onStopPublisher = f
}

func (sr *StreamRegistry) fireStopPublisher(streamPath string, reason error) {
	sr.onStopPublisherMu.Lock()
	cb := sr.onStopPublisher
	sr.onStopPublisherMu.Unlock()
	if cb != nil {
		cb(streamPath, reason)
	}
}
```

- [ ] **Step 3: 改 handleLocalPublish 在 acquire 失败时 fire**

找到 `func (sr *StreamRegistry) handleLocalPublish(streamPath string, isClusterRelay bool, registerOnDispose func(func())) {`,把现有 `if err := sr.acquire(streamPath); err != nil {` 那段替换为:

```go
	if err := sr.acquire(streamPath); err != nil {
		sr.Warn("acquire stream key failed", "streamPath", streamPath, "error", err)
		// §4.3 first-write-wins: 失败意味着另一个 peer 已经拥有该 streamPath。
		// 主动 Stop 本地 publisher,reason = ErrStreamPathTaken。
		// 注:网络抖动也可能导致 acquire 失败,本 spec v1 把所有失败都当 "key 已被占",
		// 实际生产里这是保守做法 —— 真的撞键 vs 网络抖动,效果都是 publisher 重连。
		sr.fireStopPublisher(streamPath, ErrStreamPathTaken)
		// localStreams 已记录,session 重建时仍会试 rebind;但我们已经请求 Stop,
		// publisher 不久后会消失,localStreams 在 dispose hook 里也会被清。
		// 不主动从 localStreams 删,避免与 dispose 钩子竞争。
	}
```

- [ ] **Step 4: 跑 GREEN**

```bash
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/ -run TestStreamRegistry_HandleLocalPublishStopsPublisher -count=1 -v 2>&1 | tail -5
```
Expected: `PASS`

- [ ] **Step 5: 全套回归**

```bash
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/... -count=1 2>&1 | tail -3
```
Expected: `ok`

- [ ] **Step 6: commit**

```bash
git add plugin/cluster/streamregistry.go plugin/cluster/streamregistry_test.go
git commit -m "feat(cluster): StreamRegistry first-write-wins 失败时 Stop publisher

§4.3 决策:同一 streamPath 在两节点同时被推流时,谁先 KV.Acquire 谁是 origin,
另一节点的 publisher 必须断流。改 handleLocalPublish 在 acquire 失败时 fire
SetOnStopPublisher 回调,实际 Stop 动作由 cluster 主插件挂在该回调里完成。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: 恢复 Phase 3 relay 残件(buildPullURL)

**Files:**
- Restore: `plugin/cluster/relay.go`, `plugin/cluster/relay_test.go`

- [ ] **Step 1: 把 Task 0 stash 的内容拿回来**

```bash
git stash pop
git status
```
Expected: `plugin/cluster/relay.go`, `plugin/cluster/relay_test.go` 重新出现在 untracked

- [ ] **Step 2: 验证 buildPullURL 测试还是绿**

```bash
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/ -run TestRelay_BuildPullURL -count=1 -v 2>&1 | tail -8
```
Expected: 4 个 buildPullURL 测试全 PASS

- [ ] **Step 3: commit**

```bash
git add plugin/cluster/relay.go plugin/cluster/relay_test.go
git commit -m "feat(cluster): Phase 3 起点 — buildPullURL helper + 4 unit tests

恢复 brainstorming 之前已 GREEN 的 buildPullURL:按 RelayProtocols
优先级 + peer.Advertise 表选 RTMP/RTSP/FLV 协议,拼完整 pull URL。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: ensureRelay helper — skip 分支 RED

**Files:**
- Test: `plugin/cluster/relay_test.go`

- [ ] **Step 1: 写 RED**

加到 `plugin/cluster/relay_test.go` 末尾:

```go
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
```

注意: test 顶部 import 区可能要加 `"net/url"`(因为 OnSubscribe 的 args 类型是 url.Values)。
但本测试传 nil,所以暂时不需要。

- [ ] **Step 2: 跑 RED**

```bash
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/ -run TestRelay_EnsureRelay_Skips -count=1 -v 2>&1 | tail -8
```
Expected: 编译失败 `p.relayHook undefined`, `p.OnSubscribe undefined`

---

## Task 8: ensureRelay — GREEN(skip 分支)

**Files:**
- Modify: `plugin/cluster/relay.go`
- Modify: `plugin/cluster/index.go`

- [ ] **Step 1: 在 relay.go 顶部加 import 和 hook 类型 + ClusterPlugin 字段加在 index.go**

先在 `plugin/cluster/relay.go` 文件中加(import 块更新):

```go
//go:build cluster

package plugin_cluster

import (
	"fmt"
	"net/url"

	m7s "m7s.live/v5"
)

// RelayHook 是 ensureRelay 实际执行 pull-proxy 创建的注入点。生产实现走
// Server.EnsurePullProxy,测试可换成 recorder fake。
//
// 用 any 而不是 *m7s.PullProxyConfig 是 Phase 3 中间阶段的简化 —— 主插件
// 编织 conf 后直接传 *m7s.PullProxyConfig,helper 只 record。Task 10 时会
// 收窄类型(改成 *m7s.PullProxyConfig)。
type RelayHook = func(conf any) (created bool, err error)
```

(把已有的 buildPullURL 保留不动)

- [ ] **Step 2: 在 ClusterPlugin struct 加 relayHook 字段**

打开 `plugin/cluster/index.go`,找到 `type ClusterPlugin struct {` 内,在 `streamRegistry *StreamRegistry` 后加一行:

```go
	streamRegistry *StreamRegistry

	// relayHook 是 Phase 3 Relay 创建 cluster-relay pull-proxy 的注入点。
	// 默认 nil → ensureRelay 走 p.Server.EnsurePullProxy。测试可 swap。
	relayHook RelayHook
```

- [ ] **Step 3: 在 relay.go 加 OnSubscribe 方法**

在 buildPullURL 下方加:

```go
// OnSubscribe 实现 m7s.ISubscribeHookPlugin。本节点出现订阅但没有对应本地流时:
//   1. StreamRegistry.Lookup 找 origin 节点 id
//   2. 若找不到,return(订阅者会在等待队列里超时)
//   3. 若 owner == self,return(本地流,m7s 主流程自然处理)
//   4. 否则 Membership.Peer 找 origin 的 advertise 表,buildPullURL 拼 URL
//   5. relayHook(conf) 起一个 cluster-relay pull-proxy
//
// 任何步骤失败都 log Warn 后 return,不影响订阅者本身(超时由 m7s 处理)。
func (p *ClusterPlugin) OnSubscribe(streamPath string, _ url.Values) {
	if p.streamRegistry == nil || p.membership == nil {
		return
	}
	originID, ok := p.streamRegistry.Lookup(streamPath)
	if !ok {
		return
	}
	if originID == p.NodeID {
		return
	}
	peer, ok := p.membership.Peer(originID)
	if !ok {
		p.Warn("relay: origin peer not in membership table", "streamPath", streamPath, "originId", originID)
		return
	}
	proto, fullURL, err := buildPullURL(peer, streamPath, p.RelayProtocols)
	if err != nil {
		p.Warn("relay: no matching protocol", "streamPath", streamPath, "originId", originID, "error", err)
		return
	}
	if err := p.ensureRelay(originID, streamPath, proto, fullURL); err != nil {
		p.Warn("relay: ensure pull proxy failed", "streamPath", streamPath, "error", err)
	}
}

// ensureRelay 把 relay 参数组装成 PullProxyConfig 并调用注入点(默认走 Server.EnsurePullProxy)。
// 本 Task 阶段先实现 skip / shape;Task 10 收窄类型并真接 Server.EnsurePullProxy。
func (p *ClusterPlugin) ensureRelay(originID, streamPath, proto, fullURL string) error {
	hook := p.relayHook
	if hook == nil {
		// 生产路径在 Task 10 补全
		return fmt.Errorf("relayHook not configured")
	}
	conf := map[string]string{
		"originId":   originID,
		"streamPath": streamPath,
		"type":       proto,
		"url":        fullURL,
	}
	_, err := hook(conf)
	return err
}

var _ m7s.ISubscribeHookPlugin = (*ClusterPlugin)(nil)
```

- [ ] **Step 4: 跑 GREEN(skip 分支)**

```bash
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/ -run TestRelay_EnsureRelay_Skips -count=1 -v 2>&1 | tail -8
```
Expected: 2 个 skip 测试 PASS

- [ ] **Step 5: build 验证**

```bash
/opt/homebrew/bin/go build -tags cluster ./plugin/cluster/... ./example/cluster/...
/opt/homebrew/bin/go build ./plugin/cluster/... ./example/cluster/...
```
Expected: 都退出 0

- [ ] **Step 6: commit**

```bash
git add plugin/cluster/relay.go plugin/cluster/index.go plugin/cluster/relay_test.go
git commit -m "feat(cluster): Phase 3 — ClusterPlugin.OnSubscribe 骨架 + skip 分支

实现 ISubscribeHookPlugin。Lookup 未命中 / streamPath 是本地流 → 直接 return。
ensureRelay 拿到要拉的 origin + 协议 + URL 后,通过 relayHook(注入点)实际
触发。本 Task 暂不接 Server.EnsurePullProxy(Task 10 补)。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 9: ensureRelay — CreatesPullProxyWithMarker RED + GREEN

**Files:**
- Test: `plugin/cluster/relay_test.go`
- Modify: `plugin/cluster/relay.go`

- [ ] **Step 1: 写 RED**

加到 relay_test.go 末尾:

```go
// TestRelay_OnSubscribe_CreatesPullProxyWithClusterRelayMarker 是 Phase 3
// 的核心场景: 远端有流(KV: live/foo = node-other),本节点订阅 → OnSubscribe →
// 检测出 remote → 通过 relayHook 触发 pull-proxy 创建。最后验证 hook 收到的
// conf 里:
//   - StreamPath = "live/foo"
//   - Type = "rtmp"(因 Advertise.RTMP 不空且优先级最高)
//   - URL  = "rtmp://10.0.0.1:1935/live/foo"
//   - Description 以 "cluster-relay:" + originId 开头(环回防护标记)
func TestRelay_OnSubscribe_CreatesPullProxyWithClusterRelayMarker(t *testing.T) {
	client, addr := requireConsul(t)
	nodeID := uniqNodeID(t)
	streamPath := "live/" + nodeID + "-remote"
	p := startMembershipForTest(t, nodeID, addr)
	_ = startStreamRegistryForTest(t, p)

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
```

注意: test 顶部 import 区要加 `"encoding/json"`(如果之前没 import,加)。

- [ ] **Step 2: 跑 RED(应该已经能编译,但断言会失败如 captured 还没拼 originId)**

```bash
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/ -run TestRelay_OnSubscribe_CreatesPullProxy -count=1 -v 2>&1 | tail -12
```
Expected: 已经 PASS! 因为 ensureRelay 现在就把 originId 塞 conf map 里了。

如果 PASS,继续 step 4。如果 FAIL,看是哪个断言失败,调整 ensureRelay 实现到 captured map 形态匹配。

- [ ] **Step 3: 全套回归**

```bash
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/... -count=1 2>&1 | tail -3
```
Expected: `ok`

- [ ] **Step 4: commit**

```bash
git add plugin/cluster/relay_test.go
git commit -m "test(cluster): Phase 3 — OnSubscribe 创建带 cluster-relay 标记的 pull-proxy

验证当 streamPath 的 owner 是远端 peer 时,OnSubscribe 拼出正确的
type/url/originId,投递给 relayHook(后续会接到 Server.EnsurePullProxy)。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 10: ensureRelay 接到 m7s.Server.EnsurePullProxy

**Files:**
- Modify: `plugin/cluster/relay.go`

- [ ] **Step 1: 看一下 EnsurePullProxy 签名**

```bash
grep -nE "func.*EnsurePullProxy" pull_proxy.go
grep -nE "^type PullProxyConfig struct" pull_proxy.go
```
Expected: `pull_proxy.go:280:func (s *Server) EnsurePullProxy(conf *PullProxyConfig) (pullProxy IPullProxy, created bool, err error)`
PullProxyConfig 定义在 pull_proxy.go 中(无 `type ... struct` 单独行;在 `var (..PullProxyConfig struct{...})` 块里)。

- [ ] **Step 2: 改 ensureRelay 用真 PullProxyConfig 类型 + 默认走 Server**

替换 `func (p *ClusterPlugin) ensureRelay(originID, streamPath, proto, fullURL string) error`:

```go
func (p *ClusterPlugin) ensureRelay(originID, streamPath, proto, fullURL string) error {
	conf := &m7s.PullProxyConfig{
		StreamPath:  streamPath,
		Type:        proto,
		Description: ClusterRelayDescPrefix + originID,
		PullOnStart: false,
		StopOnIdle:  true,
	}
	conf.URL = fullURL
	conf.MaxRetry = 3
	conf.RetryInterval = time.Second

	if hook := p.relayHook; hook != nil {
		_, err := hook(conf)
		return err
	}
	if p.Server == nil {
		return fmt.Errorf("server not attached")
	}
	_, _, err := p.Server.EnsurePullProxy(conf)
	return err
}
```

注意:
- `conf.URL`, `conf.MaxRetry`, `conf.RetryInterval` 实际是 `conf.Pull.URL` 等(因为 PullProxyConfig 嵌入了 `config.Pull`)。检查具体字段路径,可能要写成 `conf.Pull.URL = fullURL`。先按上面写,跑 build 时若报错再 fix。
- `time` 需在 import 加上(如果还没有)
- `ClusterRelayDescPrefix` 已在 streamregistry.go 定义,同包可直接用

- [ ] **Step 3: relay.go 顶部 import 加 `"time"`**

如果还没有,在 import 块加 `"time"`。

- [ ] **Step 4: build 验证**

```bash
/opt/homebrew/bin/go build -tags cluster ./plugin/cluster/... 2>&1 | head -10
```
若报 `conf.URL undefined`,把那几行改成 `conf.Pull.URL = fullURL`, `conf.Pull.MaxRetry = 3`, `conf.Pull.RetryInterval = time.Second`,重 build。

- [ ] **Step 5: 改 captured map 测试以适应新 conf 类型**

Task 9 写的测试目前断言的是 `map[string]string`。现在 ensureRelay 改成传 `*m7s.PullProxyConfig`,测试要更新断言。打开 relay_test.go 找到 TestRelay_OnSubscribe_CreatesPullProxyWithClusterRelayMarker,替换 recorder hook 那段为:

```go
	var captured *m7s.PullProxyConfig
	p.relayHook = func(conf any) (bool, error) {
		if c, ok := conf.(*m7s.PullProxyConfig); ok {
			captured = c
		}
		return true, nil
	}

	p.OnSubscribe(streamPath, nil)

	if captured == nil {
		t.Fatalf("relayHook never called or wrong type")
	}
	if captured.StreamPath != streamPath {
		t.Errorf("StreamPath = %q, want %q", captured.StreamPath, streamPath)
	}
	if captured.Type != "rtmp" {
		t.Errorf("Type = %q, want rtmp", captured.Type)
	}
	wantURL := "rtmp://10.0.0.1:1935/" + streamPath
	if captured.Pull.URL != wantURL {
		t.Errorf("Pull.URL = %q, want %q", captured.Pull.URL, wantURL)
	}
	wantDesc := ClusterRelayDescPrefix + originID
	if captured.Description != wantDesc {
		t.Errorf("Description = %q, want %q", captured.Description, wantDesc)
	}
```

同时 import 区加 `m7s "m7s.live/v5"`(如果还没)。

同 Task 7 的 skip 测试里 `relayHook = func(conf any)` 也要确认还能编译 —— 因 hook 签名没变(`any`),应该 OK。

- [ ] **Step 6: 跑全部 relay 测试**

```bash
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/ -run TestRelay -count=1 -v 2>&1 | tail -12
```
Expected: 7 个 relay 测试全 PASS(4 个 buildPullURL + 2 skip + 1 marker)

- [ ] **Step 7: 全套回归**

```bash
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/... -count=1 2>&1 | tail -3
```
Expected: `ok`

- [ ] **Step 8: commit**

```bash
git add plugin/cluster/relay.go plugin/cluster/relay_test.go
git commit -m "feat(cluster): Phase 3 — ensureRelay 接入 Server.EnsurePullProxy

收窄 relayHook 注入类型到 *m7s.PullProxyConfig;生产路径默认走
p.Server.EnsurePullProxy;Description 注入 cluster-relay 标记
(防 C→B→A→B 环回,§3.4)。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 11: §4.2 origin 失联 → 主动 Stop pull-proxy

**Files:**
- Modify: `plugin/cluster/relay.go`
- Modify: `plugin/cluster/index.go`
- Test: `plugin/cluster/relay_test.go`

- [ ] **Step 1: 写 RED 测试**

加到 relay_test.go 末尾:

```go
// TestRelay_StreamRegistryKeyDisappears_KillsLocalPullProxy 验证 §4.2:
// 当 m7s/streams/<path> 键在 consul 上消失,Relay 应当主动 Stop 本节点上
// 该 streamPath 对应的 cluster-relay pull-proxy。
//
// 这里仍用注入点(stopHook)解耦真实 m7s pull-proxy 对象。
func TestRelay_StreamRegistryKeyDisappears_KillsLocalPullProxy(t *testing.T) {
	client, addr := requireConsul(t)
	nodeID := uniqNodeID(t)
	streamPath := "live/" + nodeID + "-vanish"
	originID := nodeID + "-origin"
	p := startMembershipForTest(t, nodeID, addr)
	_ = startStreamRegistryForTest(t, p)

	// 写 origin 节点 + streamPath。
	peerJSON, _ := json.Marshal(PeerInfo{NodeID: originID, Advertise: AdvertiseConfig{RTMP: "10.0.0.1:1935"}})
	_, _ = client.KV().Put(&consulapi.KVPair{Key: keyNode(originID), Value: peerJSON}, nil)
	_, _ = client.KV().Put(&consulapi.KVPair{Key: keyStream(streamPath), Value: []byte(originID)}, nil)

	// 等 watcher 同步。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := p.membership.Peer(originID); !ok {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if _, ok := p.streamRegistry.Lookup(streamPath); !ok {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		break
	}

	// 让 relayHook"成功"建一个 pull-proxy(实际不建,只是登记)。
	p.relayHook = func(conf any) (bool, error) { return true, nil }

	// stopHook recorder。
	stopCh := make(chan struct {
		streamPath string
		reason     error
	}, 4)
	p.stopRelayHook = func(streamPath string, reason error) {
		stopCh <- struct {
			streamPath string
			reason     error
		}{streamPath, reason}
	}

	// 触发 OnSubscribe,这条 streamPath 会被 Relay 登记为本节点正在 relay 的对象。
	p.OnSubscribe(streamPath, nil)

	// 删 stream key,等 onStreamRemoved 钩子触发。
	if _, err := client.KV().Delete(keyStream(streamPath), nil); err != nil {
		t.Fatalf("kv delete: %v", err)
	}

	select {
	case got := <-stopCh:
		if got.streamPath != streamPath {
			t.Errorf("stopRelayHook streamPath = %q, want %q", got.streamPath, streamPath)
		}
		if !errors.Is(got.reason, ErrOriginLost) {
			t.Errorf("stopRelayHook reason = %v, want ErrOriginLost", got.reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("stopRelayHook not fired within 3s after stream key delete")
	}
}
```

- [ ] **Step 2: 跑 RED**

```bash
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/ -run TestRelay_StreamRegistryKeyDisappears -count=1 -v 2>&1 | tail -8
```
Expected: 编译失败 `p.stopRelayHook undefined`

- [ ] **Step 3: 在 ClusterPlugin struct 加 stopRelayHook 字段 + 本地 relay 表**

打开 index.go,在 relayHook 下方加:

```go
	relayHook RelayHook

	// stopRelayHook 是 Phase 3 Relay 在 origin 失联(§4.2)时 Stop 本节点 cluster-relay
	// pull-proxy 的注入点。默认 nil → 生产实现走 Server pull-proxy 查找 + Stop。
	stopRelayHook func(streamPath string, reason error)

	// activeRelays 跟踪本节点上 cluster-relay 派生的 streamPath。OnSubscribe
	// 成功调 relayHook 后写入;StreamRegistry.AddOnStreamRemoved 看到删除时读
	// 来决定是否 Stop。
	activeRelaysMu sync.Mutex
	activeRelays   map[string]struct{}
```

import 区加 `"sync"`。

- [ ] **Step 4: 在 ClusterPlugin.Start 里订阅 onStreamRemoved**

打开 index.go,在 `Start()` 方法中,在 streamRegistry 启动之后(currently `if err := p.AddTask(p.streamRegistry).WaitStarted(); err != nil {...}` 之后)加:

```go
	if p.activeRelays == nil {
		p.activeRelays = make(map[string]struct{})
	}
	p.streamRegistry.AddOnStreamRemoved(func(streamPath string) {
		p.activeRelaysMu.Lock()
		_, isActive := p.activeRelays[streamPath]
		delete(p.activeRelays, streamPath)
		p.activeRelaysMu.Unlock()
		if !isActive {
			return
		}
		if hook := p.stopRelayHook; hook != nil {
			hook(streamPath, ErrOriginLost)
			return
		}
		p.stopRelayPullProxy(streamPath, ErrOriginLost)
	})
```

- [ ] **Step 5: 在 relay.go 加 stopRelayPullProxy(生产实现) + 改 ensureRelay 登记 activeRelays**

在 relay.go 加(注意:生产实现 v1 用 ID range 找匹配 description 的 pull-proxy 然后 Stop):

```go
// stopRelayPullProxy 生产路径:遍历 Server 的 pull-proxies,找 description ==
// "cluster-relay:*" 且 streamPath 匹配的那一个,Stop(reason)。
//
// 用 description 前缀 + StreamPath 双重确认,避免误杀用户自配的同名 pull-proxy。
func (p *ClusterPlugin) stopRelayPullProxy(streamPath string, reason error) {
	if p.Server == nil {
		return
	}
	p.Server.PullProxies.Range(func(proxy m7s.IPullProxy) bool {
		conf := proxy.GetConfig()
		if conf == nil {
			return true
		}
		if conf.StreamPath != streamPath {
			return true
		}
		if !strings.HasPrefix(conf.Description, ClusterRelayDescPrefix) {
			return true
		}
		// 拿到对应 pull-proxy。Stop 它。
		if t, ok := any(proxy).(task.ITask); ok {
			t.Stop(reason)
		} else {
			p.Warn("relay: pull proxy is not a task, cannot Stop", "streamPath", streamPath)
		}
		return false
	})
}
```

relay.go 顶部 import 加:
```go
	"strings"
	task "github.com/langhuihui/gotask"
```

(注意:`m7s.IPullProxy` 已在 m7s 包,本文件 import 了 `m7s "m7s.live/v5"`,可用。
`PullProxies.Range` 签名要核对,可能是 `Range(func(IPullProxy) bool)` 也可能不是 —— 看 server.go 或 pull_proxy.go 的 PullProxyManager 定义。如果签名不同,改成实际签名。
如果不存在 Range,用 `Find(func) (..., bool)`,看 pull_proxy.go:62-90。)

然后在 ensureRelay 成功后,登记 activeRelays。改 ensureRelay:

```go
func (p *ClusterPlugin) ensureRelay(originID, streamPath, proto, fullURL string) error {
	conf := &m7s.PullProxyConfig{
		StreamPath:  streamPath,
		Type:        proto,
		Description: ClusterRelayDescPrefix + originID,
		PullOnStart: false,
		StopOnIdle:  true,
	}
	conf.Pull.URL = fullURL
	conf.Pull.MaxRetry = 3
	conf.Pull.RetryInterval = time.Second

	var err error
	if hook := p.relayHook; hook != nil {
		_, err = hook(conf)
	} else if p.Server != nil {
		_, _, err = p.Server.EnsurePullProxy(conf)
	} else {
		err = fmt.Errorf("server not attached")
	}
	if err == nil {
		p.activeRelaysMu.Lock()
		if p.activeRelays == nil {
			p.activeRelays = make(map[string]struct{})
		}
		p.activeRelays[streamPath] = struct{}{}
		p.activeRelaysMu.Unlock()
	}
	return err
}
```

- [ ] **Step 6: 跑 RED → GREEN**

```bash
/opt/homebrew/bin/go build -tags cluster ./plugin/cluster/... 2>&1 | head -10
```
若 build 失败(PullProxies.Range 签名错等),按报错调整。

```bash
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/ -run TestRelay_StreamRegistryKeyDisappears -count=1 -v 2>&1 | tail -8
```
Expected: PASS

- [ ] **Step 7: 全套回归**

```bash
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/... -count=1 2>&1 | tail -3
```
Expected: `ok`

- [ ] **Step 8: commit**

```bash
git add plugin/cluster/relay.go plugin/cluster/index.go plugin/cluster/relay_test.go
git commit -m "feat(cluster): Phase 3 — origin 失联时主动 Stop cluster-relay pull-proxy

订阅 StreamRegistry.AddOnStreamRemoved,当 m7s/streams/<path> 在 consul
上消失,立即 Stop 本节点对应的 cluster-relay pull-proxy(用 description
前缀 + streamPath 双重确认),避免等 MaxRetry 耗尽的延迟。reason 报为
ErrOriginLost。§4.2 决策。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 12: §4.3 接入 cluster 主插件的 publisher Stop

**Files:**
- Modify: `plugin/cluster/index.go`

- [ ] **Step 1: 在 ClusterPlugin.Start 里挂 onStopPublisher 回调**

打开 index.go,在 Start() 的 streamRegistry.AddOnStreamRemoved 之后(Task 11 加的那段下方)加:

```go
	p.streamRegistry.SetOnStopPublisher(func(streamPath string, reason error) {
		if p.Server == nil {
			return
		}
		pub, ok := p.Server.Streams.Get(streamPath)
		if !ok {
			return
		}
		pub.Stop(reason)
	})
```

注意:`p.Server.Streams.Get(streamPath)` 签名要核对 —— 看 server.go 中 Streams 的类型(WaitMap?),`Get` 返回 `(*Publisher, bool)`。

- [ ] **Step 2: build 验证**

```bash
/opt/homebrew/bin/go build -tags cluster ./plugin/cluster/... 2>&1 | head -10
```
若 build 失败,核对 Server.Streams.Get 签名。

- [ ] **Step 3: 全套回归(此 Task 没有专门测试,§4.3 失败路径已在 Task 5 测过)**

```bash
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/... -count=1 2>&1 | tail -3
```
Expected: `ok`

- [ ] **Step 4: commit**

```bash
git add plugin/cluster/index.go
git commit -m "feat(cluster): Phase 3 收尾 — 接入 StreamRegistry onStopPublisher

StreamRegistry 在 first-write-wins 失败时 fire onStopPublisher;cluster
主插件挂 Server.Streams.Get + pub.Stop(ErrStreamPathTaken) 实际断流。
§4.3 闭环完成。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 13: Phase 3 完工 checkpoint

- [ ] **Step 1: 全套测试 + race detector**

```bash
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -race -tags cluster ./plugin/cluster/... -count=1 2>&1 | tail -5
```
Expected: `ok`,race 干净

- [ ] **Step 2: build with + without cluster tag**

```bash
/opt/homebrew/bin/go build -tags cluster ./plugin/cluster/... ./example/cluster/...
/opt/homebrew/bin/go build ./plugin/cluster/... ./example/cluster/...
```
Expected: 都退出 0

- [ ] **Step 3: 更新 task 系统**

(由 subagent / executing-plans skill 处理,更新 TaskList 中 #5 标 completed)

---

## Task 14: Phase 4 起 — StreamLocator 接口探索

**Files:**
- 读 only: `grpc_api_route.go`, `pkg/redirect/*.go` (or similar)

- [ ] **Step 1: 找 m7s.StreamRouter 接口定义**

```bash
grep -rnE "type StreamRouter|interface.*Route.*streamPath" --include='*.go' | grep -v _test.go | head -10
grep -nE "RouteInterceptor" grpc_api_route.go server.go 2>&1 | head -10
```
记录定义位置(spec §2.4 提到 grpc_api_route.go:319)。

- [ ] **Step 2: 找 m7s.RedirectAdvisorV2 接口定义**

```bash
grep -rnE "type RedirectAdvisorV2|interface.*Redirect" --include='*.go' | grep -v _test.go | head -10
```

- [ ] **Step 3: 把发现的接口 method 签名记到 streamlocator.go 顶部注释**

Create `plugin/cluster/streamlocator.go`:

```go
//go:build cluster

package plugin_cluster

// StreamLocator 实现 m7s.StreamRouter + m7s.RedirectAdvisorV2 两个核心接口,
// 把 Consul 数据源接到 m7s 已有的 gRPC 录制 API 路由 + HTTP 回看重定向链路。
//
// 接口签名(从 m7s 核心代码抄过来,以确认无误):
//
// 待 Step 1/2 填入。
//
// 实现思路:
//   1. StreamRouter.RouteFor(streamPath) -> 通过 StreamRegistry.Lookup 找 origin,
//      若 owner == self 返回 nil(本地处理),否则返回 origin 节点的 gRPC 地址
//   2. RedirectAdvisorV2 类似,但返回 origin 的 HTTP 地址
//
// 注入点:
//   - routeHook func(streamPath) (target *PeerInfo, ok bool) 默认 = Lookup + Peer
//   - 主要为了让 Phase 4 测试 swap recorder fake
```

- [ ] **Step 4: commit 探索成果**

```bash
git add plugin/cluster/streamlocator.go
git commit -m "wip(cluster): Phase 4 — streamlocator.go 占位 + 接口探索注释

下一步把 m7s.StreamRouter / m7s.RedirectAdvisorV2 接口 method 抄进注释,
然后按 TDD 一个一个测试 + 实现。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

> **Note**: Task 14 是 Phase 4 的探索性 Task,产出是 wip commit。下一步 Task 15 把
> StreamLocator 类型 + locateTarget helper 写出来。如果 Step 1/2 grep 没找到 m7s 现成接口
> (项目可能没实现),那 Phase 4 设计要回过头改 —— **此时停止,询问用户**。

---

## Task 15: locateTarget helper — RED + GREEN

**Files:**
- Modify: `plugin/cluster/streamlocator.go`
- Test: `plugin/cluster/streamlocator_test.go`

(本 Task 假设 Task 14 已找到 m7s 接口。如果接口不存在,本 Task 改成只实现 locateTarget pure helper,等用户决策如何接入。)

- [ ] **Step 1: 写 RED 测试**

Create `plugin/cluster/streamlocator_test.go`:

```go
//go:build cluster

package plugin_cluster

import (
	"testing"
	"time"

	consulapi "github.com/hashicorp/consul/api"
)

// TestStreamLocator_LocalStreamReturnsNil 验证:streamPath 的 owner == self 时,
// locateTarget 必须返回 (nil, false) —— "本地流,不需要路由"。
func TestStreamLocator_LocalStreamReturnsNil(t *testing.T) {
	client, addr := requireConsul(t)
	nodeID := uniqNodeID(t)
	streamPath := "live/" + nodeID + "-local"
	p := startMembershipForTest(t, nodeID, addr)
	_ = startStreamRegistryForTest(t, p)

	// 让 Lookup 返回 self。
	_, _ = client.KV().Put(&consulapi.KVPair{
		Key: keyStream(streamPath), Value: []byte(nodeID),
	}, nil)
	waitForLookup(t, p, streamPath, nodeID)

	loc := &StreamLocator{plugin: p}
	target, isRemote := loc.locateTarget(streamPath)
	if isRemote {
		t.Fatalf("locateTarget returned isRemote=true for local stream")
	}
	if target != nil {
		t.Fatalf("locateTarget returned non-nil target = %+v for local stream", target)
	}
}

// TestStreamLocator_RemoteStreamReturnsPeer 验证: streamPath owner != self 时,
// 返回 (peer, true) 且 peer.NodeID = owner。
func TestStreamLocator_RemoteStreamReturnsPeer(t *testing.T) {
	client, addr := requireConsul(t)
	nodeID := uniqNodeID(t)
	originID := nodeID + "-origin"
	streamPath := "live/" + nodeID + "-remote"
	p := startMembershipForTest(t, nodeID, addr)
	_ = startStreamRegistryForTest(t, p)

	// 写 peer + stream。
	peerJSON := []byte(`{"nodeId":"` + originID + `","advertise":{"grpc":"10.0.0.1:50051"}}`)
	_, _ = client.KV().Put(&consulapi.KVPair{Key: keyNode(originID), Value: peerJSON}, nil)
	_, _ = client.KV().Put(&consulapi.KVPair{Key: keyStream(streamPath), Value: []byte(originID)}, nil)
	waitForLookup(t, p, streamPath, originID)
	waitForPeer(t, p, originID)

	loc := &StreamLocator{plugin: p}
	target, isRemote := loc.locateTarget(streamPath)
	if !isRemote {
		t.Fatalf("locateTarget returned isRemote=false for remote stream")
	}
	if target == nil || target.NodeID != originID {
		t.Fatalf("target = %+v, want NodeID=%q", target, originID)
	}
}

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
```

- [ ] **Step 2: 跑 RED**

```bash
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/ -run TestStreamLocator -count=1 -v 2>&1 | tail -8
```
Expected: 编译失败 `StreamLocator undefined`, `locateTarget undefined`

- [ ] **Step 3: 写 GREEN**

Replace `plugin/cluster/streamlocator.go` content:

```go
//go:build cluster

package plugin_cluster

// StreamLocator 实现 m7s.StreamRouter + m7s.RedirectAdvisorV2 两个核心接口,
// 把 Consul 数据源接到 m7s 已有的 gRPC 录制 API 路由 + HTTP 回看重定向链路。
//
// v1 接口的具体绑定在 Task 16 完成(把 RouteFor/RedirectFor 方法挂上去)。
// 本 Task 先实现可单测的 locateTarget pure helper。
type StreamLocator struct {
	plugin *ClusterPlugin
}

// locateTarget 看 streamPath 的 owner:
//   - 找不到 / owner == self → (nil, false)("本地处理")
//   - owner != self → (origin peer, true)
func (l *StreamLocator) locateTarget(streamPath string) (*PeerInfo, bool) {
	if l.plugin == nil || l.plugin.streamRegistry == nil || l.plugin.membership == nil {
		return nil, false
	}
	owner, ok := l.plugin.streamRegistry.Lookup(streamPath)
	if !ok {
		return nil, false
	}
	if owner == l.plugin.NodeID {
		return nil, false
	}
	peer, ok := l.plugin.membership.Peer(owner)
	if !ok {
		return nil, false
	}
	return peer, true
}
```

- [ ] **Step 4: 跑 GREEN**

```bash
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/ -run TestStreamLocator -count=1 -v 2>&1 | tail -8
```
Expected: 2 个 PASS

- [ ] **Step 5: commit**

```bash
git add plugin/cluster/streamlocator.go plugin/cluster/streamlocator_test.go
git commit -m "feat(cluster): Phase 4 — StreamLocator.locateTarget helper

Pure helper: streamPath → (peer, isRemote)。本地流返回 (nil, false),
远端流返回该 peer。Task 16 把这个 helper 接到 m7s.StreamRouter /
RedirectAdvisorV2 接口实现上。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 16: 注册 StreamLocator 到 m7s 核心接口

**Files:**
- Modify: `plugin/cluster/streamlocator.go`
- Modify: `plugin/cluster/index.go`

> **本 Task 依赖 Task 14 找到的接口签名。如果 m7s 核心没有 StreamRouter / RedirectAdvisorV2
> 接口,**stop**,告知用户:spec §2.4 假设有这些接口,但实际不存在 —— 需要补 m7s 核心
> patch(超出 cluster 插件范围),用户决策。

- [ ] **Step 1: 在 streamlocator.go 加 RouteFor / RedirectFor 方法**

(具体方法签名来自 Task 14 探索结果。下面是占位假设,**Step 0 必须用真实签名替换**:)

```go
// RouteFor 实现 m7s.StreamRouter (假设的签名: 返回 target gRPC address + bool)。
// Phase 4 用:gRPC RouteInterceptor 拦截录制 API 时,用本方法把请求路由到 origin 节点。
func (l *StreamLocator) RouteFor(streamPath string) (target string, isRemote bool) {
	peer, ok := l.locateTarget(streamPath)
	if !ok {
		return "", false
	}
	return peer.Advertise.GRPC, true
}

// RedirectFor 实现 m7s.RedirectAdvisorV2 (假设的签名)。
// Phase 4 用:HTTP 回看 /download 在本节点流不在时,302 到 origin 节点的 HTTP 地址。
func (l *StreamLocator) RedirectFor(streamPath string) (target string, isRemote bool) {
	peer, ok := l.locateTarget(streamPath)
	if !ok {
		return "", false
	}
	// 优先用 FLV (已经是带 scheme 的 http://) 端口做 HTTP 入口
	if peer.Advertise.FLV != "" {
		return peer.Advertise.FLV, true
	}
	return "", false
}
```

- [ ] **Step 2: 在 ClusterPlugin.Start 注册 StreamLocator**

打开 index.go,在 Start 末尾(在所有 task / hook 注册完之后)加:

```go
	locator := &StreamLocator{plugin: p}
	// 把 locator 接到 m7s 核心: 实际接入方法看 m7s 框架 API,可能是
	//   p.Server.RegisterStreamRouter(locator)
	// 或:
	//   p.Server.RouteInterceptor.SetStreamRouter(locator)
	// 用 Task 14 探索得到的接口名替换下面占位。
	_ = locator // TODO Task 14/16
```

(注:如果接入点尚不清楚,Step 2 暂留 `_ = locator`,等 Task 14 完整探索)

- [ ] **Step 3: 跑全套测试**

```bash
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/... -count=1 2>&1 | tail -3
```
Expected: `ok`

- [ ] **Step 4: commit**

```bash
git add plugin/cluster/streamlocator.go plugin/cluster/index.go
git commit -m "feat(cluster): Phase 4 — StreamLocator 实现 RouteFor / RedirectFor

挂到 m7s 核心 RouteInterceptor / RedirectAdvisorV2 槽位(接入点
依 Task 14 探索结果填)。本节点是 origin 返回 isRemote=false,
其余情况返回对端 advertise 地址。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 17: 删除 plugin/apiroute/ + 替换 APIRoute 配置

**Files:**
- Delete: `plugin/apiroute/` 整目录
- Modify: `pkg/config/types.go`
- Modify: `example/cluster/main.go`(如果 import 了 apiroute)

- [ ] **Step 1: 看 apiroute 被谁引用**

```bash
grep -rnE 'plugin/apiroute|APIRoute' --include='*.go' | grep -v "^plugin/apiroute/" | head -20
```

- [ ] **Step 2: 修改 pkg/config/types.go**

找到 `APIRoute` 字段(可能在 config struct 里),改名或删除。具体改法看 grep 结果。一般是:

```go
// Before:
type Config struct {
    ...
    APIRoute APIRouteConfig
    ...
}
// After:
type Config struct {
    ...
    // APIRoute 配置已废弃,功能由 plugin/cluster 接管。
    // 旧配置项启动期会被忽略 + log Info 提示。
    ...
}
```

如果旧 yaml 字段不再有意义,保留兼容(silently ignore):用 `,omitempty` 的废弃 struct + log Info 提示,但不再 wire 到任何插件。

- [ ] **Step 3: 改 example/cluster/main.go**

```bash
grep -n "apiroute" example/cluster/main.go
```
若有 `_ "m7s.live/v5/plugin/apiroute"`,删那一行。

- [ ] **Step 4: 删整目录**

```bash
git rm -r plugin/apiroute/
```

- [ ] **Step 5: build 验证**

```bash
/opt/homebrew/bin/go build ./...
/opt/homebrew/bin/go build -tags cluster ./...
```
Expected: 退出 0

- [ ] **Step 6: 全套测试**

```bash
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -tags cluster ./... -count=1 2>&1 | tail -10
```
Expected: 全 ok(可能要忽略 crypto 包 pre-existing 编译错,用 `go test -tags cluster ./plugin/cluster/... ./pkg/...`)

- [ ] **Step 7: commit**

```bash
git add -A
git commit -m "refactor(cluster): 删除 plugin/apiroute/ + 替换 APIRoute 配置项

apiroute 的功能被 plugin/cluster/streamlocator(Phase 4)完全替代。
旧 config.yaml 中 apiroute 段会被静默忽略,启动期 log Info 提示
迁移到 cluster.xxx 配置(D1 决策)。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 18: Phase 5 — mp4_streams 加 node_id 列

**Files:**
- Modify: `plugin/mp4/pkg/record.go` 或定义 mp4_streams 模型的文件
- Need to grep first

- [ ] **Step 1: 找 mp4_streams 模型定义**

```bash
grep -rnE "mp4_streams|type.*Stream.*struct.*gorm" --include='*.go' plugin/mp4/ | head -10
```

- [ ] **Step 2: 加 NodeID 列**

打开发现的模型文件,加字段:

```go
// NodeID 标识这条录制是哪个节点上 origin 拉取并写盘的。Phase 5 跨节点回看用。
// cluster 未启用时为空字符串。GORM AutoMigrate 处理迁移。
NodeID string `gorm:"index"`
```

- [ ] **Step 3: 在 Recorder 创建 Stream 行时写入 NodeID**

找到 Recorder.Run 或 Recorder 把元数据写进 DB 的地方,加:

```go
if p := r.Plugin; p != nil {
    // 借 m7s.Server 上的 cluster 插件读 NodeID(可选,未启用时空字符串)
    // ... 具体读 NodeID 的方法看 plugin 注册路径
}
```

详细实现要看 mp4 plugin 现有结构。占位代码:

```go
stream.NodeID = getClusterNodeID(p.Server)  // 待 helper 实现
```

并加 helper:

```go
// 实际位置可放在 plugin/cluster/exports.go,或者 plugin/mp4/pkg 内一个小 file
func getClusterNodeID(s *m7s.Server) string {
    // 通过 s.Plugins.Find 找 cluster 插件,读 NodeID 字段
    // ...
}
```

- [ ] **Step 4: build + 全套测试**

```bash
/opt/homebrew/bin/go build -tags 'cluster postgres' ./...
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/... -count=1 2>&1 | tail -3
```

- [ ] **Step 5: commit**

```bash
git add plugin/mp4 plugin/cluster
git commit -m "feat(mp4,cluster): mp4_streams 加 node_id 列

Phase 5 准备:跨节点回看需要知道录制是哪个节点产生的。
GORM AutoMigrate 加列,旧行 node_id 空。cluster 未启用时
node_id 始终空,不影响单机部署。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 19: Phase 5 — /download/* 跨节点 302 拦截

**Files:**
- Modify: `plugin/mp4/index.go`

- [ ] **Step 1: 找 download handler**

```bash
grep -nE "download|Download" plugin/mp4/index.go | head -10
```
Expected: 行号 ~77-81 是 `/download/{streamPath...}` handler(spec §2.5 引用)

- [ ] **Step 2: 在 handler 开头加 cluster 路由分支**

```go
func (p *MP4Plugin) handleDownload(w http.ResponseWriter, r *http.Request) {
    // ... 已有逻辑 (path 解析等)

    // 拼出 streamPath
    streamPath := /* 现有解析逻辑 */

    // Phase 5: cluster 启用且本流不在本节点 → 302 到 origin 节点
    if cluster := findClusterPlugin(p.Server); cluster != nil {
        if owner, ok := cluster.streamRegistry.Lookup(streamPath); ok && owner != cluster.NodeID {
            if peer, ok := cluster.membership.Peer(owner); ok && peer.Advertise.FLV != "" {
                target := peer.Advertise.FLV + r.URL.Path
                if r.URL.RawQuery != "" {
                    target += "?" + r.URL.RawQuery
                }
                http.Redirect(w, r, target, http.StatusFound)
                return
            }
        }
    }

    // ... 已有逻辑
}
```

`findClusterPlugin` 在 plugin/mp4 包中加(或 plugin/cluster 包提供 export helper)。

- [ ] **Step 3: build + 单元测试(本 Task 主要靠 e2e Task 26 验证)**

```bash
/opt/homebrew/bin/go build -tags cluster ./...
```

- [ ] **Step 4: commit**

```bash
git add plugin/mp4 plugin/cluster
git commit -m "feat(mp4,cluster): Phase 5 — /download 跨节点 302 兜底

本节点不持有该 streamPath 的录制时,通过 cluster.streamRegistry
找到 origin 节点的 advertise HTTP 端口,302 到该节点的同路径。
未启用 cluster / 找不到 owner 走原下载逻辑。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 20: Phase 5 — SQLite + cluster 启动期 Warn

**Files:**
- Modify: `plugin/cluster/index.go`

- [ ] **Step 1: 在 Start() 检测 DB 类型**

```go
// 在 Start() 末尾、return 之前加:
if p.Server != nil && p.Server.DB != nil {
    if dbType := /* 看 server DB 类型,例如 p.Server.DB.Dialector.Name() */; dbType == "sqlite" {
        p.Warn("cluster + SQLite 不建议生产用",
            "reason", "跨节点录制元数据(mp4_streams)不会自动共享。建议切到 PostgreSQL(P4 决策)")
    }
}
```

- [ ] **Step 2: build + 单元测试**

```bash
/opt/homebrew/bin/go build -tags 'cluster sqlite' ./...
```

- [ ] **Step 3: commit**

```bash
git add plugin/cluster/index.go
git commit -m "feat(cluster): Phase 5 — cluster + SQLite 启动期 Warn

P4 决策:不拒启,只 Warn。SQLite 用户可以 'cluster 试用',
生产场景文档明确要求 PostgreSQL。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 21: Phase 5 — 共享存储部署 docs

**Files:**
- Modify: `doc_CN/arch/cluster.md`

- [ ] **Step 1: 加新章节"共享存储部署"**

在 cluster.md 末尾(§十二 阶段进度跟踪之前)加新章节:

```markdown
## 共享存储部署(Phase 5)

集群模式下,**强烈推荐** 所有节点共享一份业务 DB + 对象存储。否则跨节点
录制元数据和文件回看会不可见。

### PostgreSQL(业务 + 录制元数据)

[详细的 docker / k8s 部署示例,connection string 配置等]

### S3 / COS / OSS(录制文件)

[详细的 minio docker 例子, m7s 配置 storage 段示例]

### 部署一致性 checklist

- [ ] 所有节点 DSN 指向同一 PostgreSQL 实例
- [ ] 所有节点 storage 配置指向同一 bucket
- [ ] cluster.nodeid 全局唯一
- [ ] cluster.advertise.{rtmp,rtsp,flv,grpc} 是其他节点能访问的地址
```

补全具体配置 yaml 片段。

- [ ] **Step 2: commit**

```bash
git add doc_CN/arch/cluster.md
git commit -m "docs(cluster): Phase 5 — 共享存储部署详解

PostgreSQL + S3/COS/OSS 配置示例,部署一致性 checklist。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 22: Phase 6 — MetricsConfig + LoadReporter 骨架

**Files:**
- Modify: `plugin/cluster/config.go`
- Create: `plugin/cluster/metrics.go`

- [ ] **Step 1: 加 MetricsConfig**

在 config.go 中加:

```go
type MetricsConfig struct {
    ReportInterval time.Duration `default:"5s" desc:"LoadReporter 上报周期"`
}
```

在 ClusterPlugin 中加 `Metrics MetricsConfig` 字段(index.go)。

- [ ] **Step 2: 创建 metrics.go 骨架**

```go
//go:build cluster

package plugin_cluster

import (
    "encoding/json"
    "runtime"
    "time"

    consulapi "github.com/hashicorp/consul/api"
    task "github.com/langhuihui/gotask"
)

// LoadReporter 周期把本节点指标写到 m7s/nodes/<self> 的 Metrics 字段。
// task.TickTask 实现,interval 由 ClusterPlugin.Metrics.ReportInterval 控制。
//
// v1 范围指标:streams, subscribers, goroutines。CPU / 带宽 v2。
type LoadReporter struct {
    task.TickTask
    plugin *ClusterPlugin
}

func (r *LoadReporter) GetTickInterval() time.Duration {
    if r.plugin == nil {
        return 5 * time.Second
    }
    return r.plugin.Metrics.ReportInterval
}

func (r *LoadReporter) Tick(_ any) {
    if err := r.report(); err != nil {
        r.Warn("metrics report failed", "error", err)
    }
}

// report 采集本节点指标 + 用当前 session 重写 m7s/nodes/<self>。
func (r *LoadReporter) report() error {
    plugin := r.plugin
    if plugin == nil || plugin.membership == nil {
        return nil
    }
    sid := plugin.membership.SessionID()
    if sid == "" {
        return nil
    }

    metrics := r.collectMetrics()

    pi := PeerInfo{
        NodeID:    plugin.NodeID,
        Advertise: plugin.Advertise,
        Metrics:   metrics,
        Version:   "",
        StartedAt: 0,  // 不动 StartedAt 让初始注册时的值保留;真实流程见 Step 4
    }
    if plugin.Meta != nil {
        pi.Version = plugin.Meta.Version
    }
    value, err := json.Marshal(pi)
    if err != nil {
        return err
    }
    ok, _, err := plugin.membership.client.KV().Acquire(&consulapi.KVPair{
        Key: keyNode(plugin.NodeID), Value: value, Session: sid,
    }, nil)
    if err != nil {
        return err
    }
    if !ok {
        return errors.New("kv acquire returned false")
    }
    return nil
}

// collectMetrics 收集 v1 范围指标。
func (r *LoadReporter) collectMetrics() map[string]any {
    m := map[string]any{
        "goroutines": runtime.NumGoroutine(),
    }
    if r.plugin != nil && r.plugin.Server != nil {
        m["streams"] = r.plugin.Server.Streams.Length()
        // subscribers: 暂留 placeholder,需要确认 m7s 是否有全局 subscriber 计数。
        // m["subscribers"] = ...
    }
    return m
}
```

`errors` import 加上。

> **注**: `Server.Streams.Length()` 签名要核对。如果不存在 Length,用 Range count。

- [ ] **Step 3: build 验证**

```bash
/opt/homebrew/bin/go build -tags cluster ./plugin/cluster/...
```

- [ ] **Step 4: 关于"重写 m7s/nodes/<self> 不丢 StartedAt"**

实际实现需要先读旧值再 merge,或者在 PeerInfo 里区分"不可变字段"(StartedAt, Version)缓存在 plugin 上,Reporter 只更新可变字段(Metrics)。简化做法:在 Membership 中暴露一个 `UpdateMetrics(metrics map[string]any) error`,内部读旧值 + 合并 + Acquire。

加到 membership.go:

```go
// UpdateMetrics 让 LoadReporter 周期更新 m7s/nodes/<self> 的 Metrics 字段,
// 不丢其他不可变字段(StartedAt 等)。
func (m *Membership) UpdateMetrics(metrics map[string]any) error {
    sid := m.SessionID()
    if sid == "" {
        return errors.New("no session yet")
    }
    pair, _, err := m.client.KV().Get(keyNode(m.plugin.NodeID), nil)
    if err != nil {
        return err
    }
    var pi PeerInfo
    if pair != nil && len(pair.Value) > 0 {
        if e := json.Unmarshal(pair.Value, &pi); e != nil {
            // 损坏的话,用最小可恢复的值重建
            pi = PeerInfo{NodeID: m.plugin.NodeID, Advertise: m.plugin.Advertise}
            if m.plugin.Meta != nil {
                pi.Version = m.plugin.Meta.Version
            }
        }
    }
    pi.Metrics = metrics
    value, err := json.Marshal(pi)
    if err != nil {
        return err
    }
    ok, _, err := m.client.KV().Acquire(&consulapi.KVPair{
        Key: keyNode(m.plugin.NodeID), Value: value, Session: sid,
    }, nil)
    if err != nil {
        return err
    }
    if !ok {
        return errors.New("kv acquire returned false")
    }
    return nil
}
```

然后 metrics.go report() 简化为:

```go
func (r *LoadReporter) report() error {
    if r.plugin == nil || r.plugin.membership == nil {
        return nil
    }
    return r.plugin.membership.UpdateMetrics(r.collectMetrics())
}
```

- [ ] **Step 5: commit**

```bash
git add plugin/cluster/config.go plugin/cluster/metrics.go plugin/cluster/membership.go plugin/cluster/index.go
git commit -m "feat(cluster): Phase 6 — LoadReporter 骨架 + Membership.UpdateMetrics

LoadReporter 周期采集 goroutines + streams,通过 Membership.UpdateMetrics
合并到 m7s/nodes/<self>(读旧值 merge metrics 后 Acquire,不丢
Version/StartedAt)。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 23: Phase 6 — LoadReporter 测试

**Files:**
- Create: `plugin/cluster/metrics_test.go`

- [ ] **Step 1: 写 RED**

```go
//go:build cluster

package plugin_cluster

import (
    "encoding/json"
    "testing"
    "time"
)

// TestLoadReporter_UpdatesMetricsField 启动 LoadReporter,等一个 tick,
// 从 consul 读 m7s/nodes/<self>,验证 Metrics 字段有 goroutines。
func TestLoadReporter_UpdatesMetricsField(t *testing.T) {
    client, addr := requireConsul(t)
    nodeID := uniqNodeID(t)
    p := startMembershipForTest(t, nodeID, addr)
    p.Metrics.ReportInterval = 100 * time.Millisecond

    reporter := &LoadReporter{plugin: p}
    if err := testRoot.AddTask(reporter).WaitStarted(); err != nil {
        t.Fatalf("start reporter: %v", err)
    }
    t.Cleanup(func() { reporter.Stop(task.ErrTaskComplete); _ = reporter.WaitStopped() })

    // 等一个 tick + 一点裕量。
    time.Sleep(300 * time.Millisecond)

    pair, _, err := client.KV().Get(keyNode(nodeID), nil)
    if err != nil { t.Fatalf("kv get: %v", err) }
    if pair == nil { t.Fatalf("key absent") }
    var pi PeerInfo
    if err := json.Unmarshal(pair.Value, &pi); err != nil {
        t.Fatalf("unmarshal: %v", err)
    }
    if _, ok := pi.Metrics["goroutines"]; !ok {
        t.Fatalf("metrics.goroutines absent, got %+v", pi.Metrics)
    }
}
```

import 顶部加 `task "github.com/langhuihui/gotask"`。

- [ ] **Step 2: 跑测试**

```bash
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/ -run TestLoadReporter -count=1 -v 2>&1 | tail -8
```
Expected: PASS

- [ ] **Step 3: commit**

```bash
git add plugin/cluster/metrics_test.go
git commit -m "test(cluster): Phase 6 — LoadReporter 写入 metrics 字段

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 24: Phase 6 — /api/cluster/lb-suggest 4 分支

**Files:**
- Modify: `plugin/cluster/lb.go`
- Modify: `plugin/cluster/metrics_test.go`(加 4 个测试)

- [ ] **Step 1: 写 RED 4 个测试**

加到 metrics_test.go:

```go
// TestLBSuggest_NoPeersReturns503: 没有任何其他 peer(只有 self,或所有 peer 都没 metrics)→ 503。
func TestLBSuggest_NoPeersReturns503(t *testing.T) {
    _, addr := requireConsul(t)
    nodeID := uniqNodeID(t)
    p := startMembershipForTest(t, nodeID, addr)
    _ = startStreamRegistryForTest(t, p)
    // 不启动 LoadReporter,self 没 metrics;也不注入其他 peer。

    rr := httptest.NewRecorder()
    req := httptest.NewRequest("GET", "/api/cluster/lb-suggest", nil)
    p.handleLBSuggest(rr, req)
    if rr.Code != 503 {
        t.Errorf("status = %d, want 503; body=%s", rr.Code, rr.Body.String())
    }
}

// TestLBSuggest_PicksLeastLoadedPeer:注入两 peer(streams=5/streams=2)→ 返回 streams=2 那个。
func TestLBSuggest_PicksLeastLoadedPeer(t *testing.T) {
    client, addr := requireConsul(t)
    nodeID := uniqNodeID(t)
    p := startMembershipForTest(t, nodeID, addr)
    _ = startStreamRegistryForTest(t, p)

    seedPeer := func(id string, streams int) {
        pi := PeerInfo{NodeID: id, Advertise: AdvertiseConfig{}, Metrics: map[string]any{"streams": streams}}
        b, _ := json.Marshal(pi)
        _, _ = client.KV().Put(&consulapi.KVPair{Key: keyNode(id), Value: b}, nil)
    }
    seedPeer(nodeID+"-A", 5)
    seedPeer(nodeID+"-B", 2)
    waitForPeer(t, p, nodeID+"-A")
    waitForPeer(t, p, nodeID+"-B")

    rr := httptest.NewRecorder()
    req := httptest.NewRequest("GET", "/api/cluster/lb-suggest", nil)
    p.handleLBSuggest(rr, req)
    if rr.Code != 200 { t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String()) }
    var resp struct{ Suggested string }
    _ = json.Unmarshal(rr.Body.Bytes(), &resp)
    if resp.Suggested != nodeID+"-B" {
        t.Errorf("suggested = %q, want %q", resp.Suggested, nodeID+"-B")
    }
}

// TestLBSuggest_TieBreaksByGoroutines: 两 peer streams 相同, goroutines 不同 → 选少的。
func TestLBSuggest_TieBreaksByGoroutines(t *testing.T) {
    client, addr := requireConsul(t)
    nodeID := uniqNodeID(t)
    p := startMembershipForTest(t, nodeID, addr)
    _ = startStreamRegistryForTest(t, p)

    seedPeer := func(id string, streams, goroutines int) {
        pi := PeerInfo{NodeID: id, Metrics: map[string]any{"streams": streams, "goroutines": goroutines}}
        b, _ := json.Marshal(pi)
        _, _ = client.KV().Put(&consulapi.KVPair{Key: keyNode(id), Value: b}, nil)
    }
    seedPeer(nodeID+"-A", 3, 200)
    seedPeer(nodeID+"-B", 3, 100)
    waitForPeer(t, p, nodeID+"-A")
    waitForPeer(t, p, nodeID+"-B")

    rr := httptest.NewRecorder()
    req := httptest.NewRequest("GET", "/api/cluster/lb-suggest", nil)
    p.handleLBSuggest(rr, req)
    var resp struct{ Suggested string }
    _ = json.Unmarshal(rr.Body.Bytes(), &resp)
    if resp.Suggested != nodeID+"-B" {
        t.Errorf("suggested = %q, want %q", resp.Suggested, nodeID+"-B")
    }
}

// TestLBSuggest_ExcludesSelfByDefault: self 是 streams=0(最少),仍返回 peer 而非 self。
func TestLBSuggest_ExcludesSelfByDefault(t *testing.T) {
    client, addr := requireConsul(t)
    nodeID := uniqNodeID(t)
    p := startMembershipForTest(t, nodeID, addr)
    _ = startStreamRegistryForTest(t, p)

    // self 的 metrics: 通过 LoadReporter 走一遍,或直接 UpdateMetrics
    _ = p.membership.UpdateMetrics(map[string]any{"streams": 0, "goroutines": 100})

    // 注入一个有 streams 的 peer。
    peerID := nodeID + "-A"
    pi := PeerInfo{NodeID: peerID, Metrics: map[string]any{"streams": 3, "goroutines": 50}}
    b, _ := json.Marshal(pi)
    _, _ = client.KV().Put(&consulapi.KVPair{Key: keyNode(peerID), Value: b}, nil)
    waitForPeer(t, p, peerID)

    rr := httptest.NewRecorder()
    req := httptest.NewRequest("GET", "/api/cluster/lb-suggest", nil)
    p.handleLBSuggest(rr, req)
    var resp struct{ Suggested string }
    _ = json.Unmarshal(rr.Body.Bytes(), &resp)
    if resp.Suggested == nodeID {
        t.Errorf("excludeSelf failed: suggested = self = %q", nodeID)
    }
    if resp.Suggested != peerID {
        t.Errorf("suggested = %q, want %q", resp.Suggested, peerID)
    }
}
```

顶部 import 加 `"net/http/httptest"`。

- [ ] **Step 2: 写 GREEN — handleLBSuggest**

在 lb.go 加新 handler 和注册:

```go
// 在 RegisterHandler() 返回的 map 里加:
"/api/cluster/lb-suggest": p.handleLBSuggest,

// 加 method:
func (p *ClusterPlugin) handleLBSuggest(w http.ResponseWriter, r *http.Request) {
    if p.membership == nil {
        http.Error(w, "membership not ready", http.StatusServiceUnavailable)
        return
    }
    excludeSelf := true
    if v := r.URL.Query().Get("excludeSelf"); v == "false" {
        excludeSelf = false
    }

    type candidate struct {
        peer      *PeerInfo
        streams   int
        goroutines int
    }
    cands := []candidate{}
    for _, peer := range p.membership.Peers() {
        if excludeSelf && peer.NodeID == p.NodeID {
            continue
        }
        if len(peer.Metrics) == 0 {
            continue
        }
        streams := metricInt(peer.Metrics, "streams")
        gr := metricInt(peer.Metrics, "goroutines")
        cands = append(cands, candidate{peer: peer, streams: streams, goroutines: gr})
    }

    if len(cands) == 0 {
        w.Header().Set("Content-Type", "application/json; charset=utf-8")
        w.WriteHeader(http.StatusServiceUnavailable)
        _, _ = w.Write([]byte(`{"error":"no peers with metrics"}`))
        return
    }

    // 排序: streams 升序,goroutines 升序,NodeID 字典序。
    sort.Slice(cands, func(i, j int) bool {
        if cands[i].streams != cands[j].streams {
            return cands[i].streams < cands[j].streams
        }
        if cands[i].goroutines != cands[j].goroutines {
            return cands[i].goroutines < cands[j].goroutines
        }
        return cands[i].peer.NodeID < cands[j].peer.NodeID
    })

    chosen := cands[0].peer
    resp := map[string]any{
        "suggested": chosen.NodeID,
        "advertise": chosen.Advertise,
        "metrics":   chosen.Metrics,
    }
    w.Header().Set("Content-Type", "application/json; charset=utf-8")
    _ = json.NewEncoder(w).Encode(resp)
}

func metricInt(m map[string]any, key string) int {
    if v, ok := m[key]; ok {
        switch n := v.(type) {
        case int:
            return n
        case float64:  // JSON 反序列化默认 float64
            return int(n)
        }
    }
    return 0
}
```

lb.go 顶部 import 加 `"sort"`(以及 `"encoding/json"` 如果还没有)。

- [ ] **Step 3: 跑测试**

```bash
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -tags cluster ./plugin/cluster/ -run TestLBSuggest -count=1 -v 2>&1 | tail -12
```
Expected: 4 PASS

- [ ] **Step 4: 全套回归**

```bash
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -race -tags cluster ./plugin/cluster/... -count=1 2>&1 | tail -5
```
Expected: ok

- [ ] **Step 5: commit**

```bash
git add plugin/cluster/lb.go plugin/cluster/metrics_test.go
git commit -m "feat(cluster): Phase 6 — /api/cluster/lb-suggest endpoint + 4 分支测试

按 streams 升序、goroutines tie-break、NodeID 字典序兜底。
默认排除 self,?excludeSelf=false 可关。无候选返回 503。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 25: Phase 6 — 在 ClusterPlugin.Start 启动 LoadReporter

**Files:**
- Modify: `plugin/cluster/index.go`

- [ ] **Step 1: 在 Start 末尾加**

```go
    reporter := &LoadReporter{plugin: p}
    if err := p.AddTask(reporter).WaitStarted(); err != nil {
        return err
    }
```

- [ ] **Step 2: build + 全套测试**

```bash
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -race -tags cluster ./plugin/cluster/... -count=1 2>&1 | tail -3
```

- [ ] **Step 3: commit**

```bash
git add plugin/cluster/index.go
git commit -m "feat(cluster): Phase 6 收尾 — LoadReporter 挂到 plugin Start

每 Metrics.ReportInterval(默认 5s)触发一次 metrics 上报。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 26: Phase 9 — example/cluster-e2e/ Dockerfile + docker-compose

**Files:**
- Create: `example/cluster-e2e/Dockerfile`
- Create: `example/cluster-e2e/docker-compose.yml`
- Create: `example/cluster-e2e/config-node-{1,2,3}.yaml`

- [ ] **Step 1: 创建 Dockerfile**

```dockerfile
# example/cluster-e2e/Dockerfile
FROM golang:1.24-alpine AS builder
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN cd example/cluster && \
    CGO_ENABLED=0 go build -tags 'cluster postgres s3' -o /out/m7s ./main.go

FROM alpine:3.19
RUN apk add --no-cache ca-certificates ffmpeg
COPY --from=builder /out/m7s /usr/local/bin/m7s
WORKDIR /app
ENTRYPOINT ["/usr/local/bin/m7s"]
```

- [ ] **Step 2: 创建 docker-compose.yml**

```yaml
version: "3.9"
services:
  consul:
    image: hashicorp/consul:latest
    command: agent -dev -client=0.0.0.0
    ports: ["8500:8500"]
    healthcheck:
      test: ["CMD", "consul", "members"]
      interval: 5s
      timeout: 3s
      retries: 10

  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_PASSWORD: m7s
      POSTGRES_DB: m7s
    ports: ["5432:5432"]
    healthcheck:
      test: ["CMD", "pg_isready", "-U", "postgres"]
      interval: 5s

  minio:
    image: minio/minio:latest
    command: server /data --console-address ":9001"
    environment:
      MINIO_ROOT_USER: admin
      MINIO_ROOT_PASSWORD: m7sm7sm7s
    ports: ["9000:9000", "9001:9001"]
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:9000/minio/health/ready"]
      interval: 5s

  node-1:
    build:
      context: ../..
      dockerfile: example/cluster-e2e/Dockerfile
    command: ["-c", "/app/config-node-1.yaml"]
    volumes:
      - ./config-node-1.yaml:/app/config-node-1.yaml:ro
    ports:
      - "1935:1935"  # rtmp
      - "554:554"    # rtsp
      - "8081:8080"  # http
      - "50051:50051" # grpc
    depends_on:
      consul: { condition: service_healthy }
      postgres: { condition: service_healthy }
      minio: { condition: service_healthy }

  node-2:
    build:
      context: ../..
      dockerfile: example/cluster-e2e/Dockerfile
    command: ["-c", "/app/config-node-2.yaml"]
    volumes:
      - ./config-node-2.yaml:/app/config-node-2.yaml:ro
    ports:
      - "1936:1935"
      - "555:554"
      - "8082:8080"
      - "50052:50051"
    depends_on:
      consul: { condition: service_healthy }

  node-3:
    build:
      context: ../..
      dockerfile: example/cluster-e2e/Dockerfile
    command: ["-c", "/app/config-node-3.yaml"]
    volumes:
      - ./config-node-3.yaml:/app/config-node-3.yaml:ro
    ports:
      - "1937:1935"
      - "556:554"
      - "8083:8080"
      - "50053:50051"
    depends_on:
      consul: { condition: service_healthy }
```

- [ ] **Step 3: 创建 config-node-1.yaml**

```yaml
http:
  listenaddr: :8080
rtmp:
  listenaddr: :1935
rtsp:
  listenaddr: :554
grpc:
  listenaddr: :50051

postgres:
  dsn: "postgres://postgres:m7s@postgres:5432/m7s?sslmode=disable"

storage:
  type: s3
  s3:
    endpoint: http://minio:9000
    accesskey: admin
    secretkey: m7sm7sm7s
    bucket: m7s-records
    region: us-east-1

cluster:
  nodeid: node-1
  consul:
    addresses: [http://consul:8500]
    sessionttl: 10s
  advertise:
    rtmp: node-1:1935
    rtsp: node-1:554
    flv:  http://node-1:8080
    grpc: node-1:50051
  relayprotocols: [rtmp, rtsp, flv]
  metrics:
    reportinterval: 5s
```

config-node-2.yaml / -3.yaml 类似,只改 `nodeid` 和 `advertise.*` 中的 hostname。

- [ ] **Step 4: build 验证**

```bash
cd example/cluster-e2e
docker compose build node-1 2>&1 | tail -20
```

- [ ] **Step 5: commit**

```bash
git add example/cluster-e2e
git commit -m "test(cluster): Phase 9 e2e — Dockerfile + docker-compose + 3 节点配置

3 m7s 节点 + 1 consul + 1 postgres + 1 minio。每个节点的
rtmp/rtsp/http/grpc 各占独立宿主端口(1935-1937 等)。
Phase 9 smoke.sh 在这套环境上跑。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 27: Phase 9 — smoke.sh + README

**Files:**
- Create: `example/cluster-e2e/smoke.sh`
- Create: `example/cluster-e2e/README.md`

- [ ] **Step 1: 写 smoke.sh(10 个场景,见 spec §4.4)**

每个场景的 bash 实现要简洁,核心断言 `[[ $count -eq 3 ]]` 等。

```bash
#!/bin/bash
# example/cluster-e2e/smoke.sh
set -euo pipefail

COMPOSE="docker compose -f example/cluster-e2e/docker-compose.yml"

cleanup() {
    $COMPOSE down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> 启动 cluster"
$COMPOSE up -d --build
echo "==> 等待节点 ready (60s 超时)"
deadline=$(($(date +%s) + 60))
while [[ $(date +%s) -lt $deadline ]]; do
    if curl -fs http://localhost:8081/api/cluster/nodes 2>/dev/null \
       && curl -fs http://localhost:8082/api/cluster/nodes 2>/dev/null \
       && curl -fs http://localhost:8083/api/cluster/nodes 2>/dev/null; then
        break
    fi
    sleep 2
done

echo "==> 场景 1: 三节点起来"
count=$(curl -fs http://localhost:8081/api/cluster/nodes | jq '.peers | length')
[[ $count -eq 3 ]] || { echo "FAIL: peer count = $count"; exit 1; }

echo "==> 场景 2: 推 RTMP 到 node-1"
ffmpeg -re -i sample.mp4 -c copy -f flv rtmp://localhost:1935/live/foo &
FFPID=$!
sleep 3
owner=$(curl -fs http://localhost:8500/v1/kv/m7s/streams/live/foo?raw)
[[ $owner == "node-1" ]] || { echo "FAIL: owner = $owner"; exit 1; }

# ... 场景 3-10 类似
# 最后 kill ffmpeg
kill $FFPID || true

echo "==> 全场景通过"
```

(需要 sample.mp4 在 e2e 目录或 docker-compose volume mount 进去)

- [ ] **Step 2: 写 README.md**

```markdown
# Cluster v1 e2e 测试

## 一次性环境
- Docker + Docker Compose
- `sample.mp4` 放到本目录

## 跑
```bash
chmod +x smoke.sh
./smoke.sh
```

退出码 0 = 全场景通过。

## 场景清单
[列出 10 个场景及对应 spec §4.4 行号]
```

- [ ] **Step 3: 手跑一次确认场景 1-3 通过(其余可能需更多调试)**

```bash
cd example/cluster-e2e
chmod +x smoke.sh
./smoke.sh 2>&1 | tail -30
```

- [ ] **Step 4: commit**

```bash
git add example/cluster-e2e/smoke.sh example/cluster-e2e/README.md
git commit -m "test(cluster): Phase 9 e2e — smoke.sh 10 场景 + README

\$COMPOSE up + 10 个 bash 场景断言。退出码 0 = 通过。
sample.mp4 由用户自备。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 28: 全局 race detector + 重复跑

**Files:**
- (无文件改动,只验证)

- [ ] **Step 1: race detector 跑 3 次**

```bash
docker exec m7s-consul-test consul kv delete -recurse m7s/ 2>&1 | tail -1
/opt/homebrew/bin/go test -race -tags cluster ./plugin/cluster/... -count=3 2>&1 | tail -5
```
Expected: 三次全 ok。如果出现 flake,记录并 fix。

- [ ] **Step 2: build with + without cluster tag**

```bash
/opt/homebrew/bin/go build ./...
/opt/homebrew/bin/go build -tags cluster ./...
```
Expected: 都退出 0

- [ ] **Step 3: vet + fmt 检查**

```bash
/opt/homebrew/bin/go vet -tags cluster ./...
gofmt -l plugin/cluster
```
Expected: 都无输出

---

## Task 29: cluster.md 同步本 spec 决策

**Files:**
- Modify: `doc_CN/arch/cluster.md`

- [ ] **Step 1: 把本 plan 引入的新决策回写**

在 cluster.md 相应段落补:
- §4.4 升级: 主动 Stop pull-proxy(§4.2 本 spec)
- 新增 §4.3 first-write-wins 行为段
- §六 决策表新增本 spec 的 4 行(本 spec §7)

- [ ] **Step 2: 修订 §十一 风险表(本 spec §8 已经枚举完整,合并进去)**

- [ ] **Step 3: commit**

```bash
git add doc_CN/arch/cluster.md
git commit -m "docs(cluster): cluster.md 同步本 spec 决策

§4.2 主动 Stop、§4.3 first-write-wins、§7 决策表新增、§十一 风险表合并。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>"
```

---

## Task 30: 收尾 — 更新 task list + 撰写 PR 描述

- [ ] **Step 1: 所有 phase 任务标 completed**

通过 subagent / executing-plans 把 #5, #6, #7, #8, #9 任务标 completed。

- [ ] **Step 2: 准备 PR 描述(给 reviewer 看的)**

```markdown
## Summary

cluster v1: Phase 3-6 + e2e 完成。

- **Phase 3 Relay**: ISubscribeHookPlugin → buildPullURL → ensureRelay(EnsurePullProxy)。
  + origin 失联主动 Stop pull-proxy(§4.2)
  + first-write-wins 强制 Stop publisher(§4.3)
- **Phase 4 StreamLocator**: 替代 plugin/apiroute,接入 m7s 现有 RouteInterceptor / RedirectAdvisorV2。
- **Phase 5 跨节点回看**: mp4 /download/* 302 兜底 + 共享存储部署 docs。
- **Phase 6 LoadReporter + LB Suggest**: 周期 metrics + /api/cluster/lb-suggest 4 分支。
- **Phase 9 e2e**: docker-compose 3 node + smoke.sh 10 场景。

Spec: docs/superpowers/specs/2026-05-12-cluster-design.md
Plan: docs/superpowers/plans/2026-05-12-cluster-implementation.md

## Test plan
- [x] `go test -race -tags cluster ./plugin/cluster/... -count=3` 全绿
- [x] `go build -tags cluster ./...` + `go build ./...` 都 OK
- [x] `cd example/cluster-e2e && ./smoke.sh` 退出 0
```

---

## Self-Review Notes

Self-review done(skill 要求,inline 已修)。覆盖核查:

| Spec 章节 | Plan 任务 |
|---|---|
| §1 架构概览 | (背景,无对应 task) |
| §2.1 Membership | Phase 1 已完成 + Task 22 加 UpdateMetrics |
| §2.2 StreamRegistry | Phase 2 已完成 + Task 3 加 AddOnStreamRemoved + Task 5 加 SetOnStopPublisher |
| §2.3 Relay | Task 6-13 |
| §2.4 StreamLocator | Task 14-17 |
| §2.5 LoadReporter | Task 22-25 |
| §2.6 跨节点回看 | Task 18-21 |
| §2.7 HTTP 端点 | Phase 1+2 已完成 + Task 24 加 lb-suggest |
| §3 数据模型 | 分散在各 Task |
| §4.1 Auth 部署约束 | Task 21 docs |
| §4.2 Origin 失联 | Task 11 |
| §4.3 first-write-wins | Task 4-5 + Task 12 |
| §4.4 e2e 10 场景 | Task 26-27 |
| §5 横切关注点 | (已实现 / 散布在 task) |
| §6.1 三层金字塔 | Task 28(race) + 各 phase test |
| §6.2 测试清单 | 每 Task 含 TDD |
| §6.4 迁移清单 | Task 17 |
| §6.5 验收硬指标 | Task 28-30 |

**已知 Plan 风险**:
- **Task 14-17(StreamLocator)依赖 m7s 核心已经有 StreamRouter / RedirectAdvisorV2 接口。如果实际查证发现没有,Phase 4 设计需要回到 brainstorming 阶段**(spec §2.4 提到 grpc_api_route.go:319 的 `RouteInterceptor`,但 spec 未确认对应 Go 接口的具体签名)。Task 14 显式包含探索 step,并要求 stop+询问用户。
- **Task 18-19(mp4 改动)对 mp4 插件具体 handler 路径假设。**实际位置需要 Step 1 grep 验证。
- **Task 22 中 m7s.Server.Streams.Length() 签名假设。**未确认,build 报错后调整。

---

## Execution Handoff

**Plan complete and saved to `docs/superpowers/plans/2026-05-12-cluster-implementation.md`.**

两种执行方式选一个:

**1. Subagent-Driven (推荐)** — REQUIRED SUB-SKILL: `superpowers:subagent-driven-development`。每 Task 派一个新 subagent 执行 + 复核 + 推进。前面 sub-agent 被看门狗杀过的经验:**每个 subagent 的 prompt 必须明确单 Task,不让它一次读巨量文件**。

**2. Inline Execution** — REQUIRED SUB-SKILL: `superpowers:executing-plans`。在当前 session 直接走 Task 1 → Task 30,每 N 个 Task 一个 checkpoint 给你确认。

哪个?
