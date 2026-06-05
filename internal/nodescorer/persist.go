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

// saveStateLocked persists scorer counters to disk. **CALLER MUST HOLD
// s.mu** (write lock — read won't satisfy because the caller has typically
// just mutated s.state). Failures are logged but not fatal.
//
// Why no internal locking: this is called from score() which already holds
// s.mu.Lock(). A naive RLock here self-deadlocks because Go's RWMutex
// blocks RLock when readerCount has been negated by a Lock — even if the
// Lock is held by the same goroutine. Caught in production 2026-06-06:
// every nodescorer goroutine on 89 / 92 had been hung at saveState.RLock
// for 70+ minutes since the previous deploy, blocking every caller of
// GetSnapshot (handleStatus, handleNodesHealth) by extension.
//
// The disk write happens while the caller's write lock is still held.
// JSON marshal + atomic write of a small map is sub-millisecond on local
// disk — acceptable lock-hold time for our scoring cadence.
func (s *Scorer) saveStateLocked() {
	if s.renderer == nil {
		return
	}
	raw := make(map[string]persistedState, len(s.state))
	for tag, st := range s.state {
		raw[tag] = persistedState{
			Strikes:  st.strikes,
			OkRounds: st.okRuns,
		}
	}

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
