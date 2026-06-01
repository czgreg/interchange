package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

func (s *Server) handleRefreshIntervalGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"seconds": int64(s.deps.Scheduler.Interval().Seconds()),
	})
}

func (s *Server) handleRefreshIntervalPut(w http.ResponseWriter, r *http.Request) {
	var dto struct {
		Seconds *int64 `json:"seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&dto); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if dto.Seconds == nil {
		http.Error(w, "seconds field is required", http.StatusBadRequest)
		return
	}
	if *dto.Seconds < 0 {
		http.Error(w, "seconds must be >= 0", http.StatusBadRequest)
		return
	}
	d := time.Duration(*dto.Seconds) * time.Second

	if err := s.deps.Store.Mutate(s.deps.Cfg, func(c *config.Config) error {
		c.Subscribe.RefreshInterval = d
		return nil
	}); err != nil {
		http.Error(w, "persist: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.deps.Scheduler.SetInterval(d)
	writeJSON(w, http.StatusOK, map[string]any{"seconds": *dto.Seconds})
}
