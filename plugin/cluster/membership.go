//go:build cluster

package plugin_cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	consulapi "github.com/hashicorp/consul/api"
	task "github.com/langhuihui/gotask"
)

// PeerInfo 是 Consul KV `m7s/nodes/<nodeID>` 的 JSON 序列化结构。
// 也是 /api/cluster/nodes 返回给外部 LB 的载荷。
type PeerInfo struct {
	NodeID    string          `json:"nodeId"`
	Advertise AdvertiseConfig `json:"advertise"`
	Metrics   map[string]any  `json:"metrics,omitempty"`
	Version   string          `json:"version,omitempty"`
	StartedAt int64           `json:"startedAt"`
}

// Membership 是成员管理模块,负责维持本节点 Consul session
// 与监听 m7s/nodes/ 前缀。只负责 Phase 1 范围,
// 流位置(Phase 2)与跨节点拉流(Phase 3)在其它模块中。
type Membership struct {
	task.Work
	plugin *ClusterPlugin
	client *consulapi.Client

	mu        sync.RWMutex
	peers     map[string]*PeerInfo
	sessionID string

	// session 重建回调,Phase 2 将注册重新 Acquire 所有本地流的回调。
	onSessionRebuiltMu sync.Mutex
	onSessionRebuilt   []func(sessionID string)
}

func newMembership(p *ClusterPlugin) *Membership {
	return &Membership{
		plugin: p,
		peers:  make(map[string]*PeerInfo),
	}
}

// Start 在 task 系统接管后被调用,创建 Consul client 并挂上子 task。
func (m *Membership) Start() (err error) {
	cfg := consulapi.DefaultConfig()
	cfg.Address = m.plugin.Consul.Addresses[0]
	if t := m.plugin.Consul.Token; t != "" {
		cfg.Token = t
	}
	m.client, err = consulapi.NewClient(cfg)
	if err != nil {
		return fmt.Errorf("create consul client: %w", err)
	}
	m.AddTask(&sessionTask{m: m})
	m.AddTask(&nodeWatcher{m: m})
	return nil
}

// Peers 返回 peers map 的当前快照(浅拷贝切片,PeerInfo 共享指针)。
func (m *Membership) Peers() []*PeerInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*PeerInfo, 0, len(m.peers))
	for _, p := range m.peers {
		out = append(out, p)
	}
	return out
}

// Peer 按 nodeID 查 peer。Phase 3 的 relay 用这个查端口表。
func (m *Membership) Peer(nodeID string) (*PeerInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.peers[nodeID]
	return p, ok
}

// SessionID 返回当前 session ID。Phase 2 需要它做 KV.Acquire。
func (m *Membership) SessionID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessionID
}

// AddOnSessionRebuilt 注册 session 重建回调。Phase 2 在这里挂
// RebindAllStreams 的逻辑,实现 A1。
func (m *Membership) AddOnSessionRebuilt(f func(sessionID string)) {
	m.onSessionRebuiltMu.Lock()
	defer m.onSessionRebuiltMu.Unlock()
	m.onSessionRebuilt = append(m.onSessionRebuilt, f)
}

func (m *Membership) setSession(sid string) {
	m.mu.Lock()
	m.sessionID = sid
	m.mu.Unlock()
}

func (m *Membership) fireSessionRebuilt(sid string) {
	m.onSessionRebuiltMu.Lock()
	cbs := append([]func(string){}, m.onSessionRebuilt...)
	m.onSessionRebuiltMu.Unlock()
	for _, cb := range cbs {
		cb(sid)
	}
}

func (m *Membership) replacePeers(np map[string]*PeerInfo) {
	m.mu.Lock()
	m.peers = np
	m.mu.Unlock()
}

// ---------------------------------------------------------------------
// sessionTask: 创建 + 续期 Consul session,注册 m7s/nodes/<self>。
// 注意:实现为 Go() 而不是 Run()。Go() 在 goroutine 里跑,允许 Membership 下
// 的兄弟 task(nodeWatcher 等)并行运行;Run() 会同步阻塞父 Job 的事件循环,
// 后续兄弟 task 永远跑不到。重试不依赖框架 SetRetry(对 TaskGo 无效),改成
// runOnce 失败后 Go() 内部循环重建 session。
// ---------------------------------------------------------------------

type sessionTask struct {
	task.Task
	m *Membership
}

func (s *sessionTask) Go() error {
	for {
		if s.Err() != nil {
			return task.ErrTaskComplete
		}
		if err := s.runOnce(); err != nil {
			s.Warn("session error, will retry", "error", err)
			select {
			case <-s.Done():
				return task.ErrTaskComplete
			case <-time.After(s.m.plugin.Consul.SessionRenewInterval):
			}
		}
	}
}

// runOnce 跑一次完整的"创建 session → 注册节点 → 续期循环"。
// 任何一步出错,destroy 当前 sid 后返回;Go() 负责退避后重新建立 session。
func (s *sessionTask) runOnce() (err error) {
	se := &consulapi.SessionEntry{
		Name:     "m7s-cluster-" + s.m.plugin.NodeID,
		TTL:      s.m.plugin.Consul.SessionTTL.String(),
		Behavior: consulapi.SessionBehaviorDelete,
		// 注: consul 把 LockDelay=0 (Go time.Duration 零值) 当作"用 15s 默认值"。
		// 节点意外失联后,我们希望尽快(秒级)重新选取流位置,15s 太久;且
		// 同一 nodeID 只会被自己的 sessionTask 抢占,不需要长 lock-delay 防互踩。
		// 用 1ms 实质上是 0,但避免落到默认分支。
		LockDelay: time.Millisecond,
	}
	sid, _, err := s.m.client.Session().Create(se, nil)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	s.Info("consul session created", "sessionId", sid, "ttl", se.TTL)
	s.m.setSession(sid)

	defer func() {
		if err != nil {
			s.Warn("destroying session due to error", "sessionId", sid, "error", err)
			_, _ = s.m.client.Session().Destroy(sid, nil)
		}
	}()

	if err = s.registerNode(sid); err != nil {
		return fmt.Errorf("register node: %w", err)
	}

	// 每次 session 建立都通知回调(首次也通知)。Phase 2 的 RebindAllStreams
	// 在首次 fire 时 localStreams 为空、是 no-op;但保证之后的 OnPublish
	// 即使在 session 还没就绪时就发生,也能在 session 就绪后被 rebind 兜底。
	s.m.fireSessionRebuilt(sid)

	ticker := time.NewTicker(s.m.plugin.Consul.SessionRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.Done():
			_, _ = s.m.client.Session().Destroy(sid, nil)
			return nil
		case <-ticker.C:
			// 注: consul/api Session().Renew 在 session 已被外部 Destroy/超时时
			// 返回 (nil, nil, nil)—— 不是 error。光 check err 会让 Renew 循环
			// 永远跑不出来、新 session 永远不建。必须同时检查 entry == nil。
			entry, _, err := s.m.client.Session().Renew(sid, nil)
			if err != nil {
				return fmt.Errorf("renew session: %w", err)
			}
			if entry == nil {
				return fmt.Errorf("session %s no longer exists on consul", sid)
			}
		}
	}
}

func (s *sessionTask) registerNode(sid string) error {
	pi := &PeerInfo{
		NodeID:    s.m.plugin.NodeID,
		Advertise: s.m.plugin.Advertise,
		Version:   s.m.plugin.Meta.Version,
		StartedAt: time.Now().Unix(),
	}
	value, err := json.Marshal(pi)
	if err != nil {
		return fmt.Errorf("marshal node info: %w", err)
	}
	ok, _, err := s.m.client.KV().Acquire(&consulapi.KVPair{
		Key:     keyNode(s.m.plugin.NodeID),
		Value:   value,
		Session: sid,
	}, nil)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("kv acquire returned false; another session may hold the key")
	}
	return nil
}

// ---------------------------------------------------------------------
// nodeWatcher: blocking query 循环监听 m7s/nodes/ 前缀。
// 每次 List 都用 WaitIndex + WaitTime 长轮询,变更或超时返回。
//
// 实现为 Go(),原因同 sessionTask: Run() 会同步阻塞父 Job 事件循环,
// 导致和 sessionTask 互斥不能并行。网络错误时内部自行退避 2s 再试,
// 不依赖框架 SetRetry。
// ---------------------------------------------------------------------

type nodeWatcher struct {
	task.Task
	m         *Membership
	lastIndex uint64
}

func (w *nodeWatcher) Go() error {
	for {
		if w.Err() != nil {
			return task.ErrTaskComplete
		}
		opts := (&consulapi.QueryOptions{
			WaitIndex: w.lastIndex,
			WaitTime:  w.m.plugin.Consul.WaitTime,
		}).WithContext(w)
		pairs, meta, err := w.m.client.KV().List(prefixNodes, opts)
		if err != nil {
			if w.Err() != nil {
				return task.ErrTaskComplete
			}
			w.Warn("watch error, will retry", "prefix", prefixNodes, "error", err)
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

func (w *nodeWatcher) refresh(pairs consulapi.KVPairs) {
	np := make(map[string]*PeerInfo, len(pairs))
	for _, p := range pairs {
		nodeID := strings.TrimPrefix(p.Key, prefixNodes)
		if nodeID == "" || strings.Contains(nodeID, "/") {
			continue
		}
		var pi PeerInfo
		if err := json.Unmarshal(p.Value, &pi); err != nil {
			w.Warn("decode peer info", "key", p.Key, "error", err)
			continue
		}
		np[nodeID] = &pi
	}
	w.m.replacePeers(np)
}

const (
	prefixNodes   = "m7s/nodes/"
	prefixStreams = "m7s/streams/"
)

func keyNode(nodeID string) string {
	return prefixNodes + nodeID
}
