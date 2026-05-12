//go:build cluster

package plugin_cluster

import (
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	consulapi "github.com/hashicorp/consul/api"
	task "github.com/langhuihui/gotask"
	m7s "m7s.live/v5"
)

// uniqNodeID 把 t.Name() 转成 consul KV 路径安全的 nodeID,确保多个测试
// 之间的 m7s/nodes/<id> 永不撞键(否则 race 导致下一个测试的 Acquire 失败)。
func uniqNodeID(t *testing.T) string {
	t.Helper()
	name := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	return "test-" + name
}

// testRoot 是 plugin/cluster 测试用的任务根。Membership 和 StreamRegistry
// 在生产代码里挂在 ClusterPlugin (经由 m7s.Plugin) 下;在测试里我们绕开
// 完整 m7s.Server,直接把它们挂到这个迷你 root 下,生命周期约束不变。
var (
	testRoot     task.RootManager[uint32, task.ManagerItem[uint32]]
	testRootOnce sync.Once
)

func ensureTestRoot() {
	testRootOnce.Do(func() {
		testRoot.Init()
	})
}

// consulTestAddr 返回测试 consul HTTP 地址。优先 env var,默认 127.0.0.1:8500。
// 启动方式见 plugin/cluster/README.md(或 docker run hashicorp/consul agent -dev)。
func consulTestAddr() string {
	if v := os.Getenv("CONSUL_HTTP_ADDR"); v != "" {
		return v
	}
	return "127.0.0.1:8500"
}

// requireConsul 在 consul 不可达时 skip 而不 fail。CI/本机没启 docker 也能跑构建检查,
// 只是跳过集成层测试。
func requireConsul(t *testing.T) (*consulapi.Client, string) {
	t.Helper()
	addr := consulTestAddr()

	// 先 TCP 探测,失败比创建 client 后 status 探测快得多。
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		t.Skipf("consul not reachable at %s (set CONSUL_HTTP_ADDR or run `docker run --rm -d --name m7s-consul-test -p 8500:8500 hashicorp/consul:latest agent -dev -client=0.0.0.0`): %v", addr, err)
	}
	_ = conn.Close()

	cfg := consulapi.DefaultConfig()
	cfg.Address = addr
	client, err := consulapi.NewClient(cfg)
	if err != nil {
		t.Fatalf("consul client: %v", err)
	}

	// status 检测一次,确认 agent 起来了。
	if _, err := client.Status().Leader(); err != nil {
		t.Skipf("consul at %s not ready (no leader): %v", addr, err)
	}

	cleanConsulState(t, client)
	t.Cleanup(func() { cleanConsulState(t, client) })

	return client, addr
}

// cleanConsulState 删 m7s/nodes/ 与 m7s/streams/ 全部键,并销毁所有名字以
// "m7s-cluster-" 开头的 session。保证测试之间无残留状态。
func cleanConsulState(t *testing.T, client *consulapi.Client) {
	t.Helper()
	for _, prefix := range []string{prefixNodes, prefixStreams} {
		if _, err := client.KV().DeleteTree(prefix, nil); err != nil {
			t.Fatalf("clean prefix %s: %v", prefix, err)
		}
	}
	sessions, _, err := client.Session().List(nil)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	for _, s := range sessions {
		if strings.HasPrefix(s.Name, "m7s-cluster-") {
			if _, err := client.Session().Destroy(s.ID, nil); err != nil {
				t.Fatalf("destroy lingering session %s: %v", s.ID, err)
			}
		}
	}
}

// startMembershipForTest 构造一个最小 ClusterPlugin + Membership,挂到 testRoot
// 下并等到一个稳定的注册状态:keyNode(self) 在 Consul 上存在,且 session 字段
// 与当前 SessionID() 一致(等同于 registerNode 已成功)。
//
// 不能仅等 SessionID() 非空: sessionTask 先 setSession 再 registerNode,
// 若 registerNode 失败、任务重试,wait 会拿到已被 Destroy 的死 sid。
func startMembershipForTest(t *testing.T, nodeID, consulAddr string) *ClusterPlugin {
	t.Helper()
	ensureTestRoot()

	plugin := &ClusterPlugin{
		NodeID: nodeID,
		Consul: ConsulConfig{
			Addresses:            []string{consulAddr},
			WaitTime:             200 * time.Millisecond,
			SessionTTL:           10 * time.Second,
			SessionRenewInterval: 200 * time.Millisecond,
		},
		Advertise: AdvertiseConfig{RTMP: "127.0.0.1:1935"},
	}
	// 生产路径下 m7s.InstallPlugin 会写好 Meta;测试绕过框架,这里手动补一个
	// 空 PluginMeta,避免 sessionTask.registerNode 读 Meta.Version 时 nil deref。
	plugin.Meta = &m7s.PluginMeta{Version: "test"}
	plugin.membership = newMembership(plugin)
	if err := testRoot.AddTask(plugin.membership).WaitStarted(); err != nil {
		t.Fatalf("start membership: %v", err)
	}
	// Stop 是异步的;一定要 WaitStopped 等 sessionTask 彻底退出 + session 销毁,
	// 否则它的 renew 循环可能跑过下个测试的 setup 边界,造成交叉污染。
	t.Cleanup(func() {
		plugin.membership.Stop(task.ErrTaskComplete)
		_ = plugin.membership.WaitStopped()
	})

	// 单独的 client 用来探测稳定状态,避免和 membership 的 client 互相干扰。
	cfg := consulapi.DefaultConfig()
	cfg.Address = consulAddr
	probe, err := consulapi.NewClient(cfg)
	if err != nil {
		t.Fatalf("probe client: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sid := plugin.membership.SessionID()
		if sid != "" {
			pair, _, err := probe.KV().Get(keyNode(nodeID), nil)
			if err == nil && pair != nil && pair.Session == sid {
				return plugin
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("membership did not stabilize (key %s with current sid) within 5s", keyNode(nodeID))
	return nil
}

// startStreamRegistryForTest 在已有 plugin 上启动 StreamRegistry,挂到 testRoot。
func startStreamRegistryForTest(t *testing.T, p *ClusterPlugin) *StreamRegistry {
	t.Helper()
	ensureTestRoot()
	p.streamRegistry = newStreamRegistry(p)
	if err := testRoot.AddTask(p.streamRegistry).WaitStarted(); err != nil {
		t.Fatalf("start stream registry: %v", err)
	}
	t.Cleanup(func() {
		p.streamRegistry.Stop(task.ErrTaskComplete)
		_ = p.streamRegistry.WaitStopped()
	})
	return p.streamRegistry
}
