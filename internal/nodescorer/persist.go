package nodescorer

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
)

// persistedState is the subset of nodeState written to disk.
// We don't persist RTT history (that belongs to mihomo) or the
// health snapshot; only the scorer's own state machine counters.
type persistedState struct {
	Strikes  int `json:"strikes"`
	OkRounds int `json:"ok_rounds"`
}

// stateFile returns the JSON path in the same directory as the config file
// (already writable per systemd ReadWritePaths).
func (s *Scorer) stateFile() string {
	dir := filepath.Dir(s.renderer.Path())
	return filepath.Join(dir, "nodescorer-state.json")
}

// loadState restores scorer counters from the previous session. Called once
// at startup, before Run. If the file is absent or malformed it is silently
// ignored — scorer starts fresh.
func (s *Scorer) loadState() {
	data, err := os.ReadFile(s.stateFile())
	if err != nil {
		return // normal: first run or renderer not set yet
	}
	var raw map[string]persistedState
	if err := json.Unmarshal(data, &raw); err != nil {
		slog.Warn("nodescorer: ignoring corrupt state file", "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for tag, ps := range raw {
		s.state[tag] = &nodeState{
			strikes: ps.Strikes,
			okRuns:  ps.OkRounds,
		}
	}
	slog.Info("nodescorer: restored state", "nodes", len(raw))
}

// saveState persists scorer counters to disk. Called after each scoring
// round. Failures are logged but not fatal.
func (s *Scorer) saveState() {
	if s.renderer == nil {
		return
	}
	s.mu.RLock()
	raw := make(map[string]persistedState, len(s.state))
	for tag, st := range s.state {
		raw[tag] = persistedState{
			Strikes:  st.strikes,
			OkRounds: st.okRuns,
		}
	}
	s.mu.RUnlock()

	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		slog.Warn("nodescorer: marshal state failed", "err", err)
		return
	}
	tmp := s.stateFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		slog.Warn("nodescorer: write state failed", "err", err)
		return
	}
	if err := os.Rename(tmp, s.stateFile()); err != nil {
		slog.Warn("nodescorer: rename state failed", "err", err)
	}
}
