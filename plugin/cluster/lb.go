//go:build cluster

package plugin_cluster

import (
	"encoding/json"
	"net/http"
)

// RegisterHandler 暴露 cluster 提供的 HTTP 端点。
//
// Phase 1: /api/cluster/nodes
// Phase 2: /api/cluster/streams
// /api/cluster/lb-suggest 在 Phase 6 补充。
func (p *ClusterPlugin) RegisterHandler() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/api/cluster/nodes":   p.handleNodes,
		"/api/cluster/streams": p.handleStreams,
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
