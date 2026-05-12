//go:build cluster

package plugin_cluster

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	consulapi "github.com/hashicorp/consul/api"
	task "github.com/langhuihui/gotask"
	m7s "m7s.live/v5"
)

// ClusterRelayDescPrefix 用于 PullProxyConfig.Description 标记一个
// pull-proxy 是 cluster relay 派生的(而非用户配置的常规拉流代理)。
//
// Phase 3 创建 cluster-relay PullProxy 时会把 Description 设置成
//   "cluster-relay:<originNodeID>"
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
}

func newStreamRegistry(p *ClusterPlugin) *StreamRegistry {
	return &StreamRegistry{
		plugin:       p,
		streams:      make(map[string]string),
		localStreams: make(map[string]struct{}),
	}
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
	isClusterRelay := pub.PullProxyConfig != nil &&
		strings.HasPrefix(pub.PullProxyConfig.Description, ClusterRelayDescPrefix)
	sr.handleLocalPublish(pub.StreamPath, isClusterRelay, pub.OnDispose)
}

// handleLocalPublish 是 OnPublish 的可测试核心。
//   - isClusterRelay: 来自 cluster-relay 派生的 publisher(Q2)直接跳过,避免环回
//   - registerOnDispose: 通常是 Publisher.OnDispose,测试可传 nil 自行管理生命周期
func (sr *StreamRegistry) handleLocalPublish(streamPath string, isClusterRelay bool, registerOnDispose func(func())) {
	if isClusterRelay || streamPath == "" {
		return
	}

	sr.localMu.Lock()
	sr.localStreams[streamPath] = struct{}{}
	sr.localMu.Unlock()

	if err := sr.acquire(streamPath); err != nil {
		// 失败不致命: localStreams 已记录,session 重建/就绪时会 rebind 兜底。
		sr.Warn("acquire stream key failed", "streamPath", streamPath, "error", err)
	}

	if registerOnDispose != nil {
		registerOnDispose(func() {
			sr.localMu.Lock()
			delete(sr.localStreams, streamPath)
			sr.localMu.Unlock()
			if err := sr.release(streamPath); err != nil {
				sr.Warn("release stream key failed", "streamPath", streamPath, "error", err)
			}
		})
	}
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
