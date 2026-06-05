package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/uxtelemetry"
)

// uxTelemetryDTO is the on-the-wire shape for both POST submission and
// GET responses. Time is server-stamped on POST so client clock skew
// can't reorder the timeline.
type uxTelemetryDTO struct {
	Time       time.Time `json:"time,omitempty"`
	Domain     string    `json:"domain"`
	EventType  string    `json:"event_type"`
	DurationMs int       `json:"duration_ms,omitempty"`
	ClientID   string    `json:"client_id,omitempty"`
	Detail     string    `json:"detail,omitempty"`
}

// handleUXTelemetryPost accepts either a single event or a batched array.
// Validation is intentionally light: this is a high-volume best-effort
// stream, not control-plane state. Bad events are dropped silently (still
// a 202 response) so a misbehaving client doesn't impede everyone else.
func (s *Server) handleUXTelemetryPost(w http.ResponseWriter, r *http.Request) {
	if s.deps.UXTel == nil {
		http.Error(w, "telemetry disabled", http.StatusServiceUnavailable)
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "body read: "+err.Error(), http.StatusBadRequest)
		return
	}
	trimmed := bytes.TrimLeft(body, " \t\r\n")

	var events []uxTelemetryDTO
	switch {
	case len(trimmed) > 0 && trimmed[0] == '[':
		if err := json.Unmarshal(body, &events); err != nil {
			http.Error(w, "invalid json array: "+err.Error(), http.StatusBadRequest)
			return
		}
	case len(trimmed) > 0 && trimmed[0] == '{':
		var single uxTelemetryDTO
		if err := json.Unmarshal(body, &single); err != nil {
			http.Error(w, "invalid json object: "+err.Error(), http.StatusBadRequest)
			return
		}
		events = []uxTelemetryDTO{single}
	default:
		http.Error(w, "expected JSON object or array", http.StatusBadRequest)
		return
	}

	added := 0
	for _, dto := range events {
		if dto.Domain == "" || dto.EventType == "" {
			continue
		}
		s.deps.UXTel.Add(uxtelemetry.Event{
			Domain:     dto.Domain,
			EventType:  dto.EventType,
			DurationMs: dto.DurationMs,
			ClientID:   dto.ClientID,
			Detail:     dto.Detail,
		})
		added++
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": added})
}

// handleUXTelemetryGet returns the most recent events. Query params:
//
//	limit      — max events to return (default 100, hard cap 1000)
//	since_sec  — only events newer than (now - since_sec); default 3600
func (s *Server) handleUXTelemetryGet(w http.ResponseWriter, r *http.Request) {
	if s.deps.UXTel == nil {
		http.Error(w, "telemetry disabled", http.StatusServiceUnavailable)
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	sinceSec := 3600
	if v := r.URL.Query().Get("since_sec"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			sinceSec = n
		}
	}
	since := time.Now().Add(-time.Duration(sinceSec) * time.Second)
	events := s.deps.UXTel.Snapshot(limit, since)
	writeJSON(w, http.StatusOK, map[string]any{
		"events": events,
		"count":  len(events),
	})
}

// handleUXTelemetrySummary returns per-domain aggregates: event counts,
// p50/p95 TTFB, error rate. Used by ops dashboards. Default window 1h.
func (s *Server) handleUXTelemetrySummary(w http.ResponseWriter, r *http.Request) {
	if s.deps.UXTel == nil {
		http.Error(w, "telemetry disabled", http.StatusServiceUnavailable)
		return
	}
	windowSec := 3600
	if v := r.URL.Query().Get("window_sec"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			windowSec = n
		}
	}
	summary := s.deps.UXTel.Aggregate(time.Duration(windowSec) * time.Second)
	writeJSON(w, http.StatusOK, map[string]any{
		"window_sec": windowSec,
		"domains":    summary,
	})
}
