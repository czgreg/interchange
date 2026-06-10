package api

import "net/http"

// handleNodesHealth returns the latest NodeScorer snapshot: per-node RTT
// stats + passive throughput + qualified/in-pool status + summary.
// Requires engine=mihomo + node_qualify.enabled=true; returns 503 otherwise.
func (s *Server) handleNodesHealth(w http.ResponseWriter, _ *http.Request) {
	if s.deps.NodeScorer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "node health scoring not active (requires engine=mihomo + node_qualify.enabled=true)",
		})
		return
	}
	writeJSON(w, http.StatusOK, s.deps.NodeScorer.GetSnapshot())
}
