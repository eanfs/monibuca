//go:build cluster

package plugin_cluster

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	consulapi "github.com/hashicorp/consul/api"
	task "github.com/langhuihui/gotask"
)

// TestLoadReporter_UpdatesMetricsField: 启动 LoadReporter,等一个 tick,
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
	if err != nil {
		t.Fatalf("kv get: %v", err)
	}
	if pair == nil {
		t.Fatalf("key absent")
	}
	var pi PeerInfo
	if err := json.Unmarshal(pair.Value, &pi); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := pi.Metrics["goroutines"]; !ok {
		t.Fatalf("metrics.goroutines absent, got %+v", pi.Metrics)
	}
}

// TestLBSuggest_NoPeersReturns503: 没有任何 peer 有 metrics → 503。
func TestLBSuggest_NoPeersReturns503(t *testing.T) {
	_, addr := requireConsul(t)
	nodeID := uniqNodeID(t)
	p := startMembershipForTest(t, nodeID, addr)
	_ = startStreamRegistryForTest(t, p)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/cluster/lb-suggest", nil)
	p.handleLBSuggest(rr, req)
	if rr.Code != 503 {
		t.Errorf("status = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
}

// TestLBSuggest_PicksLeastLoadedPeer: 两 peer streams 不同 → 选少的。
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
	if rr.Code != 200 {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Suggested string `json:"suggested"`
	}
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
	var resp struct {
		Suggested string `json:"suggested"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Suggested != nodeID+"-B" {
		t.Errorf("suggested = %q, want %q", resp.Suggested, nodeID+"-B")
	}
}

// TestLBSuggest_ExcludesSelfByDefault: self 是 streams 最少,仍返回 peer 而非 self。
func TestLBSuggest_ExcludesSelfByDefault(t *testing.T) {
	client, addr := requireConsul(t)
	nodeID := uniqNodeID(t)
	p := startMembershipForTest(t, nodeID, addr)
	_ = startStreamRegistryForTest(t, p)

	// self 的 metrics 通过 UpdateMetrics 直接写入(不需启 LoadReporter)
	_ = p.membership.UpdateMetrics(map[string]any{"streams": 0, "goroutines": 100})

	peerID := nodeID + "-A"
	pi := PeerInfo{NodeID: peerID, Metrics: map[string]any{"streams": 3, "goroutines": 50}}
	b, _ := json.Marshal(pi)
	_, _ = client.KV().Put(&consulapi.KVPair{Key: keyNode(peerID), Value: b}, nil)
	waitForPeer(t, p, peerID)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/cluster/lb-suggest", nil)
	p.handleLBSuggest(rr, req)
	var resp struct {
		Suggested string `json:"suggested"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Suggested == nodeID {
		t.Errorf("excludeSelf failed: suggested = self = %q", nodeID)
	}
	if resp.Suggested != peerID {
		t.Errorf("suggested = %q, want %q", resp.Suggested, peerID)
	}
}
