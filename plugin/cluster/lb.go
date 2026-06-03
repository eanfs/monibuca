//go:build cluster

package plugin_cluster

import (
	"encoding/json"
	"net/http"
	"sort"
)

// RegisterHandler 暴露 cluster 提供的 HTTP 端点。
//
// Phase 1: /api/cluster/nodes
// Phase 2: /api/cluster/streams
// Phase 6: /api/cluster/lb-suggest
func (p *ClusterPlugin) RegisterHandler() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/api/cluster/nodes":      p.handleNodes,
		"/api/cluster/streams":    p.handleStreams,
		"/api/cluster/lb-suggest": p.handleLBSuggest,
	}
}

func (p *ClusterPlugin) handleNodes(w http.ResponseWriter, r *http.Request) {
	if p.membership == nil {
		http.Error(w, "membership not ready", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	resp := map[string]any{
		"self":      p.NodeID,
		"sessionId": p.membership.SessionID(),
		"peers":     p.membership.Peers(),
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		p.Warn("encode /api/cluster/nodes", "error", err)
	}
}

func (p *ClusterPlugin) handleStreams(w http.ResponseWriter, r *http.Request) {
	if p.streamRegistry == nil {
		http.Error(w, "stream registry not ready", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	resp := map[string]any{
		"self":    p.NodeID,
		"streams": p.streamRegistry.Streams(),
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		p.Warn("encode /api/cluster/streams", "error", err)
	}
}

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
		peer       *PeerInfo
		streams    int
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
		cands = append(cands, candidate{
			peer:       peer,
			streams:    metricInt(peer.Metrics, "streams"),
			goroutines: metricInt(peer.Metrics, "goroutines"),
		})
	}

	if len(cands) == 0 {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"no peers with metrics"}`))
		return
	}

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
		case float64: // JSON 反序列化默认 float64
			return int(n)
		}
	}
	return 0
}
