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

// handlePoolState returns the manual-mode pool state: yaml baseline,
// current effective pool, divergence flag, and recent emergency events.
// In auto / unset mode the baseline / chain are empty — the operator can
// still inspect the structure without errors.
func (s *Server) handlePoolState(w http.ResponseWriter, _ *http.Request) {
	if s.deps.NodeScorer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "node health scoring not active (requires engine=mihomo + node_qualify.enabled=true)",
		})
		return
	}
	writeJSON(w, http.StatusOK, s.deps.NodeScorer.GetPoolState())
}

// handlePoolClearEmergency reverts the effective pool to the yaml
// baseline, dropping all emergency-driven mutations. The next scoring
// round detects the change and triggers exactly one hot-reload to push
// the reverted pool to mihomo. Operator's "I've handled it" signal.
//
// Returns the post-clear PoolState for confirmation.
func (s *Server) handlePoolClearEmergency(w http.ResponseWriter, _ *http.Request) {
	if s.deps.NodeScorer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "node health scoring not active (requires engine=mihomo + node_qualify.enabled=true)",
		})
		return
	}
	s.deps.NodeScorer.ClearEmergency()
	writeJSON(w, http.StatusOK, s.deps.NodeScorer.GetPoolState())
}
