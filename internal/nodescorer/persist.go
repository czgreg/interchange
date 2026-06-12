package nodescorer

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// persistedState is the subset of nodeState written to disk.
// We don't persist RTT history (that belongs to mihomo) or the
// health snapshot; only the scorer's own state machine counters.
type persistedState struct {
	Strikes       int       `json:"strikes"`
	OkRounds      int       `json:"ok_rounds"`
	HardFailStart time.Time `json:"hard_fail_start,omitempty"`
}

// stateFileV3 is the on-disk format v3 — adds the pool-transitions audit
// log + rollback quarantine map on top of v2's emergency state. v2 files
// auto-upgrade in place on the next save. The EWMA map is included as
// an optional field — older v3 files without it just start with empty
// EWMAs, which behaves like "no signal yet" until probes accumulate.
type stateFileV3 struct {
	Version            int                       `json:"version"`
	Nodes              map[string]persistedState `json:"nodes"`
	EffectivePool      []string                  `json:"effective_pool,omitempty"`
	YamlBaseline       []string                  `json:"yaml_baseline,omitempty"`
	EmergencyEvents    []EmergencyEvent          `json:"emergency_events,omitempty"`
	Transitions        []PoolTransition          `json:"transitions,omitempty"`
	RollbackQuarantine map[string]time.Time      `json:"rollback_quarantine,omitempty"`
	EWMA               map[string]*nodeEWMA      `json:"ewma,omitempty"`
}

// stateFileV2 is the on-disk format v2 — wraps the per-node map in a
// container that also persists emergency-mode state. v1 (a bare
// map[string]persistedState) is loaded transparently.
type stateFileV2 struct {
	Version         int                       `json:"version"`
	Nodes           map[string]persistedState `json:"nodes"`
	EffectivePool   []string                  `json:"effective_pool,omitempty"`
	YamlBaseline    []string                  `json:"yaml_baseline,omitempty"`
	EmergencyEvents []EmergencyEvent          `json:"emergency_events,omitempty"`
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
//
// On-disk format auto-detects v1 / v2 / v3. v1 is a bare per-node map;
// v2 added the stateFileV2 envelope with emergency state; v3 added the
// transitions audit log + rollback quarantine. Older files upgrade in
// place on the next save.
func (s *Scorer) loadState() {
	data, err := os.ReadFile(s.stateFile())
	if err != nil {
		return // normal: first run or renderer not set yet
	}
	// Detect version from the envelope's `version` field (zero / missing
	// = v1 bare map).
	var probe struct {
		Version int `json:"version"`
	}
	_ = json.Unmarshal(data, &probe)

	if probe.Version >= 3 {
		var v3 stateFileV3
		if err := json.Unmarshal(data, &v3); err == nil {
			s.mu.Lock()
			defer s.mu.Unlock()
			for tag, ps := range v3.Nodes {
				s.state[tag] = &nodeState{
					strikes:       ps.Strikes,
					okRuns:        ps.OkRounds,
					hardFailStart: ps.HardFailStart,
				}
			}
			s.effectivePool = append([]string(nil), v3.EffectivePool...)
			s.yamlBaseline = append([]string(nil), v3.YamlBaseline...)
			s.emergencyEvents = append([]EmergencyEvent(nil), v3.EmergencyEvents...)
			s.transitions = append([]PoolTransition(nil), v3.Transitions...)
			if v3.RollbackQuarantine != nil {
				s.rollbackQuarantine = copyQuarantine(v3.RollbackQuarantine)
			}
			if v3.EWMA != nil {
				s.ewma = copyEWMA(v3.EWMA)
			}
			slog.Info("nodescorer: restored state v3",
				"nodes", len(v3.Nodes),
				"effective_pool", len(s.effectivePool),
				"events", len(s.emergencyEvents),
				"transitions", len(s.transitions),
				"quarantined", len(s.rollbackQuarantine))
			return
		}
	}
	if probe.Version >= 2 {
		var v2 stateFileV2
		if err := json.Unmarshal(data, &v2); err == nil {
			s.mu.Lock()
			defer s.mu.Unlock()
			for tag, ps := range v2.Nodes {
				s.state[tag] = &nodeState{
					strikes:       ps.Strikes,
					okRuns:        ps.OkRounds,
					hardFailStart: ps.HardFailStart,
				}
			}
			s.effectivePool = append([]string(nil), v2.EffectivePool...)
			s.yamlBaseline = append([]string(nil), v2.YamlBaseline...)
			s.emergencyEvents = append([]EmergencyEvent(nil), v2.EmergencyEvents...)
			slog.Info("nodescorer: restored state v2 (will upgrade to v3 on next save)",
				"nodes", len(v2.Nodes),
				"effective_pool", len(s.effectivePool),
				"events", len(s.emergencyEvents))
			return
		}
	}
	// v1 fallback: bare map[string]persistedState.
	var raw map[string]persistedState
	if err := json.Unmarshal(data, &raw); err != nil {
		slog.Warn("nodescorer: ignoring corrupt state file", "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for tag, ps := range raw {
		s.state[tag] = &nodeState{
			strikes:       ps.Strikes,
			okRuns:        ps.OkRounds,
			hardFailStart: ps.HardFailStart,
		}
	}
	slog.Info("nodescorer: restored state v1 (will upgrade to v3 on next save)",
		"nodes", len(raw))
}

// copyQuarantine deep-copies a node→until map (used for both load and
// save sides; mutating the persisted snapshot must never leak into
// runtime state).
func copyQuarantine(in map[string]time.Time) map[string]time.Time {
	out := make(map[string]time.Time, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// copyEWMA deep-copies the per-node EWMA map. Same rationale as
// copyQuarantine: persisted snapshot is decoupled from runtime state.
//
// It also SCRUBS poisoned points: a window whose smoothed value is at or
// near the no-measurement sentinel (1e9 decaying at the 24h half-life) is
// the fingerprint of the pre-2026-06-12 bug that fed compositeScore's
// no-data sentinel into the EWMA. Such a point is reset to "no signal"
// (zero UpdatedAt) so the node re-seeds from its next real measurement
// rather than carrying garbage for days. Applied on load and save, so an
// already-poisoned state file heals on the next restart instead of waiting
// out the decay.
func copyEWMA(in map[string]*nodeEWMA) map[string]*nodeEWMA {
	out := make(map[string]*nodeEWMA, len(in))
	for k, v := range in {
		if v == nil {
			continue
		}
		copied := *v
		copied.Short = sanitizeEWMAPoint(copied.Short)
		copied.Long = sanitizeEWMAPoint(copied.Long)
		out[k] = &copied
	}
	return out
}

// poisonFloor is the threshold above which a smoothed EWMA value is
// treated as sentinel-poisoned. A real composite tops out around ~25k
// (p95≈10s + 2·jitter + 5000·fail² + passive), and an EWMA is a weighted
// average of observations, so a legitimate smoothed value can never exceed
// the worst composite ever seen — i.e. it stays well under 50k. The bug
// seeded the window at 1e9; that sentinel only decays below 50k after
// ~14.3 half-lives (≈2.4 days on the 4h short window, ≈14 days on the 24h
// long window). So anything at/above 50k is decaying sentinel residue,
// never a real score. (The original 1e6 floor was too high: a short
// window's sentinel decays to ~3e5 in under 2 days and slipped through,
// leaving a freshly-reloaded node's short EWMA poisoned — caught on 92's
// first post-fix scoring round, 2026-06-12.)
const poisonFloor = 5e4

// sanitizeEWMAPoint zeroes a point whose value is sentinel-poisoned,
// turning it back into "no signal yet" (nodeLongEWMA → +Inf).
func sanitizeEWMAPoint(p ewmaPoint) ewmaPoint {
	if p.Value >= poisonFloor {
		return ewmaPoint{}
	}
	return p
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
	nodes := make(map[string]persistedState, len(s.state))
	for tag, st := range s.state {
		nodes[tag] = persistedState{
			Strikes:       st.strikes,
			OkRounds:      st.okRuns,
			HardFailStart: st.hardFailStart,
		}
	}
	v3 := stateFileV3{
		Version:            3,
		Nodes:              nodes,
		EffectivePool:      append([]string(nil), s.effectivePool...),
		YamlBaseline:       append([]string(nil), s.yamlBaseline...),
		EmergencyEvents:    append([]EmergencyEvent(nil), s.emergencyEvents...),
		Transitions:        append([]PoolTransition(nil), s.transitions...),
		RollbackQuarantine: copyQuarantine(s.rollbackQuarantine),
		EWMA:               copyEWMA(s.ewma),
	}

	data, err := json.MarshalIndent(v3, "", "  ")
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
