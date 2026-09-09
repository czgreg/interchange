package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// handlePoolTransitions returns the most-recent N pool composition
// changes (auto K-gating swaps, emergency evictions/promotions,
// operator rollbacks) plus the current rollback quarantine. ops uses
// this for audit + to identify what to roll back.
func (s *Server) handlePoolTransitions(w http.ResponseWriter, r *http.Request) {
	if s.deps.NodeScorer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "scorer not active",
		})
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"transitions": s.deps.NodeScorer.GetTransitions(limit),
		"quarantine":  s.deps.NodeScorer.GetQuarantine(),
	})
}

// rollbackDTO is POST /api/pool/rollback's request body.
type rollbackDTO struct {
	Steps              int `json:"steps"`               // default 1, max = audit log length
	QuarantineSeconds  int `json:"quarantine_seconds"`  // default 3600 (1h), max 7d
}

// handlePoolRollback un-does the last N transitions and quarantines the
// just-removed nodes for the configured duration. Triggers a re-render
// + hot-reload via RunRefresh so the change reaches mihomo immediately
// (next scoring round would too, but we want fast feedback for ops).
func (s *Server) handlePoolRollback(w http.ResponseWriter, r *http.Request) {
	if s.deps.NodeScorer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "scorer not active",
		})
		return
	}
	dto := rollbackDTO{Steps: 1, QuarantineSeconds: 3600}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&dto); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	if dto.Steps <= 0 {
		dto.Steps = 1
	}
	dur := time.Duration(dto.QuarantineSeconds) * time.Second
	if dur <= 0 {
		dur = 1 * time.Hour
	}
	res, err := s.deps.NodeScorer.Rollback(dto.Steps, dur)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": err.Error(),
		})
		return
	}
	// Trigger re-render + hot-reload so ops sees the rollback in mihomo
	// without waiting for next 5min scoring round. Background since we
	// already have the response ready.
	go func() {
		_ = RunRefresh(r.Context(), s.deps.Subscribe, s.deps.Renderer, s.deps.Controller, s.deps.Notifier)
	}()
	writeJSON(w, http.StatusOK, res)
}
