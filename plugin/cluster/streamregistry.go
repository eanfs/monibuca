//go:build cluster

package plugin_cluster

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	task "github.com/eanfs/gotask"
	consulapi "github.com/hashicorp/consul/api"
	m7s "m7s.live/v5"
)

// ClusterRelayDescPrefix 用于 PullProxyConfig.Description 标记一个
// pull-proxy 是 cluster relay 派生的(而非用户配置的常规拉流代理)。
//
// Phase 3 创建 cluster-relay PullProxy 时会把 Description 设置成
//
//	"cluster-relay:<originNodeID>"
//
// StreamRegistry.OnPublish 看到带这个前缀的 publisher 直接跳过——
// 避免环回(B 从 A 拉到流后又把流写进 Consul,导致 C 上的订阅者去 B 拉)。
const ClusterRelayDescPrefix = "cluster-relay:"

// StreamRegistry 维护本节点 publishers 在 Consul 上的位置注册,
// 同时 watch m7s/streams/ 在本地维护一份 streamPath → nodeID 表。
//
// Phase 2 范围。Phase 3 的 relay 与 Phase 4 的 StreamRouter
// 通过 Lookup() 读这张表。
type StreamRegistry struct {
	task.Work
	plugin *ClusterPlugin

	mu      sync.RWMutex
	streams map[string]string

	localMu      sync.Mutex
	localStreams map[string]struct{}

	onStreamRemovedMu sync.Mutex
	onStreamRemoved   []func(streamPath string)

	// acquireFn/releaseFn 默认指向 sr.acquire/sr.release;抽成字段是为了单测能注入
	// fake,不依赖真实 Consul。
	acquireFn func(streamPath string) error
	releaseFn func(streamPath string) error

	// dispatch 把一段工作异步投递到 StreamRegistry 自己的事件循环(默认 = 一次性
	// callbackGoTask,Run 内联执行,FIFO 串行)。OnPublish/OnDispose 用它把阻塞式
	// KV acquire/release 与 first-write-wins 冲突停流挪出 Server.Streams 事件循环
	// —— 避免重入 Streams.Call 死锁(RC1)与阻塞 Streams 事件循环(RC2);FIFO 保证
	// 同一 streamPath 的 acquire/release 有序。单测可注入同步/捕获版。
	dispatch func(func())
}

func newStreamRegistry(p *ClusterPlugin) *StreamRegistry {
	sr := &StreamRegistry{
		plugin:       p,
		streams:      make(map[string]string),
		localStreams: make(map[string]struct{}),
	}
	sr.acquireFn = sr.acquire
	sr.releaseFn = sr.release
	sr.dispatch = func(fn func()) {
		sr.AddTask(&callbackGoTask{fn: fn})
	}
	return sr
}

func (sr *StreamRegistry) Start() error {
	sr.AddTask(&streamWatcher{sr: sr})
	// session 重建钩子: 实现 A1。首次 session 就绪时也会被叫到,
	// localStreams 为空时是 no-op;之后任何 session 失效后的重建
	// 都会触发本地流位置的 re-Acquire。
	sr.plugin.membership.AddOnSessionRebuilt(func(sid string) {
		sr.rebindAll(sid)
	})
	return nil
}

// OnPublish 在 ClusterPlugin.OnPublish 转发过来时被调用。
// 把 *m7s.Publisher 解耦成简单参数后交给 handleLocalPublish。
func (sr *StreamRegistry) OnPublish(pub *m7s.Publisher) {
	// RC3 修复:relay publisher 的判据不能只看 pub.PullProxyConfig.Description ——
	// 该 Description 不可靠(EnsurePullProxy 命中已存在的同 streamPath pull-proxy 会
	// 直接返回、丢弃新 conf 的 cluster-relay 标记,见 pull_proxy.go EnsurePullProxy)。
	// 改以 cluster 插件权威自持的 activeRelays(ensureRelay 同步写入)为准,
	// Description 仅作兜底信号。
	isClusterRelay := (pub.PullProxyConfig != nil &&
		strings.HasPrefix(pub.PullProxyConfig.Description, ClusterRelayDescPrefix)) ||
		(sr.plugin != nil && sr.plugin.isActiveRelay(pub.StreamPath))
	// stop 直接绑定到该 publisher 的 Stop —— 不再经 Server.Streams.SafeGet 反查。
	// SafeGet 会重入 Server.Streams 事件循环(m.Call),而 OnPublish 本身就跑在该
	// 事件循环上,重入会永久死锁(RC1)。
	sr.handleLocalPublish(pub.StreamPath, isClusterRelay, func(reason error) {
		pub.Stop(reason)
	}, pub.OnDispose)
}

// handleLocalPublish 是 OnPublish 的可测试核心。
//   - isClusterRelay: 来自 cluster-relay 派生的 publisher(Q2)直接跳过,避免环回
//   - stop: 停掉该 publisher(生产 = pub.Stop),first-write-wins 冲突时调用
//   - registerOnDispose: 通常是 Publisher.OnDispose,测试可传 nil 自行管理生命周期
//
// ⚠️ 调用方是 Server.Streams 事件循环 goroutine。KV acquire 是阻塞式 Consul I/O,
// 冲突停流又可能重入 Streams —— 两者都必须经 sr.dispatch 异步挪走,绝不能在本函数
// 的同步调用栈里执行(否则死锁 Streams 事件循环,RC1/RC2)。
func (sr *StreamRegistry) handleLocalPublish(streamPath string, isClusterRelay bool, stop func(error), registerOnDispose func(func())) {
	if isClusterRelay || streamPath == "" {
		return
	}

	sr.localMu.Lock()
	sr.localStreams[streamPath] = struct{}{}
	sr.localMu.Unlock()

	// 每个本地 publish 一组握手标志:acquired 记录是否真正抢到了 KV 键(只有抢到了
	// 才能 release,否则 KV.Delete 会把属主 peer 的键删掉);disposed 记录 publisher
	// 是否已 dispose。acquire 跑在 StreamRegistry 循环、dispose 跑在 Streams 循环,
	// 两个事件先后不定 —— 谁后到谁负责 release,releaseOnce CAS 保证最多释放一次
	// (双重 release 的第二次 Delete 可能误删新 publisher 刚抢到的键)。
	var acquired, disposed, releaseOnce atomic.Bool

	// releaseKey 释放 KV 键。调用方必须已在 StreamRegistry 循环上(dispatch 内或
	// acquire 闭包内),不得在 Streams 事件循环上直接调用(阻塞式 Consul I/O)。
	releaseKey := func() {
		if !releaseOnce.CompareAndSwap(false, true) {
			return
		}
		if err := sr.releaseFn(streamPath); err != nil {
			sr.Warn("release stream key failed", "streamPath", streamPath, "error", err)
		}
	}

	if registerOnDispose != nil {
		registerOnDispose(func() {
			sr.localMu.Lock()
			delete(sr.localStreams, streamPath)
			sr.localMu.Unlock()
			disposed.Store(true)
			if acquired.Load() {
				// release 与 acquire 对称:阻塞式 Consul I/O 必须 dispatch 到
				// StreamRegistry 自己的循环,否则 Consul 抖动时 dispose 回调会
				// 冻结 Server.Streams 事件循环(全机 publish/subscribe 停摆)。
				// dispatch 为 FIFO 串行,天然排在同名流后续 publish 的 acquire 之前。
				sr.dispatch(releaseKey)
			}
		})
	}

	// RC1+RC2 修复:KV acquire 阻塞式 Consul I/O + first-write-wins 失败停流,异步
	// dispatch 到 StreamRegistry 自己的事件循环,彻底离开 Server.Streams 调用栈。
	sr.dispatch(func() {
		if err := sr.acquireFn(streamPath); err != nil {
			sr.Warn("acquire stream key failed", "streamPath", streamPath, "error", err)
			// §4.3 first-write-wins: 失败意味着另一个 peer 已经拥有该 streamPath。
			// 主动 Stop 本地 publisher,reason = ErrStreamPathTaken。
			// 注:网络抖动也可能导致 acquire 失败,本 spec v1 把所有失败都当 "key 已被占",
			// 实际生产里这是保守做法 —— 真的撞键 vs 网络抖动,效果都是 publisher 重连。
			if stop != nil {
				stop(ErrStreamPathTaken)
			}
			return
		}
		acquired.Store(true)
		// TOCTOU 补偿:acquire 异步完成时 publisher 可能已 dispose(秒断/Consul 慢),
		// 彼时 dispose 回调看到 acquired=false 跳过了 release。这里回查 disposed 立即
		// 补一次 release(已在本循环上,内联执行,先于队列中后续同名流的 acquire),
		// 否则键被本节点健康 session 永久持有 —— 该流名在全集群被毒化:任何节点
		// 无法再发布、订阅被错误路由到本节点,直到本节点重启。
		if disposed.Load() {
			releaseKey()
		}
	})
}

// Lookup 给 Phase 3 (relay) / Phase 4 (StreamRouter) 用,
// 返回 streamPath 当前所在的 nodeID。
func (sr *StreamRegistry) Lookup(streamPath string) (nodeID string, ok bool) {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	nodeID, ok = sr.streams[streamPath]
	return
}

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

// callbackGoTask 是一次性任务:在 StreamRegistry 自己的事件循环上 **内联(Run)**
// 跑一段回调然后完成退出。用于把 OnPublish 的阻塞式 KV acquire/release 与冲突停流
// 挪出 Server.Streams 事件循环(RC1/RC2)。
//
// 刻意用 Run() 而非 Go():同一 streamPath 的 acquire → release → 再 acquire 必须
// 严格 FIFO 串行,否则流秒断重推时,上一个 publish 的异步 release 可能删掉新
// publish 刚抢到的 KV 键。串行的代价是 Consul 慢时本队列积压,但不影响
// Server.Streams 与 streamWatcher(后者是 Go() 独立 goroutine)。
type callbackGoTask struct {
	task.Task
	fn func()
}

func (t *callbackGoTask) Run() error {
	t.fn()
	return task.ErrTaskComplete
}

// Streams 返回当前 streams map 的快照,主要给 /api/cluster/streams 用。
func (sr *StreamRegistry) Streams() map[string]string {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	out := make(map[string]string, len(sr.streams))
	for k, v := range sr.streams {
		out[k] = v
	}
	return out
}

func (sr *StreamRegistry) acquire(streamPath string) error {
	sid := sr.plugin.membership.SessionID()
	if sid == "" {
		return errors.New("no consul session yet")
	}
	ok, _, err := sr.plugin.membership.client.KV().Acquire(&consulapi.KVPair{
		Key:     keyStream(streamPath),
		Value:   []byte(sr.plugin.NodeID),
		Session: sid,
	}, nil)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("kv acquire returned false; %s may be held by another session", streamPath)
	}
	return nil
}

func (sr *StreamRegistry) release(streamPath string) error {
	sid := sr.plugin.membership.SessionID()
	key := keyStream(streamPath)
	if sid != "" {
		// 显式 Release 解除 session 绑定;在 session 仍有效时这样干净。
		if _, _, err := sr.plugin.membership.client.KV().Release(&consulapi.KVPair{
			Key:     key,
			Session: sid,
		}, nil); err != nil {
			// 不致命: 后面的 Delete 仍然会清掉键。
			sr.Debug("kv release failed", "streamPath", streamPath, "error", err)
		}
	}
	if _, err := sr.plugin.membership.client.KV().Delete(key, nil); err != nil {
		return err
	}
	return nil
}

func (sr *StreamRegistry) rebindAll(sid string) {
	sr.localMu.Lock()
	paths := make([]string, 0, len(sr.localStreams))
	for p := range sr.localStreams {
		paths = append(paths, p)
	}
	sr.localMu.Unlock()
	if len(paths) == 0 {
		return
	}
	sr.Info("rebinding local streams to new session", "count", len(paths), "sessionId", sid)
	for _, p := range paths {
		if err := sr.acquire(p); err != nil {
			sr.Warn("rebind stream failed", "streamPath", p, "error", err)
		}
	}
}

func (sr *StreamRegistry) replace(np map[string]string) {
	sr.mu.Lock()
	sr.streams = np
	sr.mu.Unlock()
}

// ---------------------------------------------------------------------
// streamWatcher: blocking query 循环监听 m7s/streams/ 前缀。
//
// 实现为 Go() 而不是 Run(),与 sessionTask/nodeWatcher 同理: Run() 同步
// 阻塞父 Job 事件循环,兄弟 task 跑不到。错误时自行退避 2s。
// ---------------------------------------------------------------------

type streamWatcher struct {
	task.Task
	sr        *StreamRegistry
	lastIndex uint64
}

func (w *streamWatcher) Go() error {
	for {
		if w.Err() != nil {
			return task.ErrTaskComplete
		}
		opts := (&consulapi.QueryOptions{
			WaitIndex: w.lastIndex,
			WaitTime:  w.sr.plugin.Consul.WaitTime,
		}).WithContext(w)
		pairs, meta, err := w.sr.plugin.membership.client.KV().List(prefixStreams, opts)
		if err != nil {
			if w.Err() != nil {
				return task.ErrTaskComplete
			}
			w.Warn("watch error, will retry", "prefix", prefixStreams, "error", err)
			select {
			case <-w.Done():
				return task.ErrTaskComplete
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if meta != nil {
			w.lastIndex = meta.LastIndex
		}
		w.refresh(pairs)
	}
}

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

func keyStream(streamPath string) string {
	return prefixStreams + streamPath
}
