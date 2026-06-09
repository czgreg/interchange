// emergency.go — auto-eviction + chain-promotion for manual pool mode.
//
// Sole-maintainer fail-safe: when a TRUSTED pool member is hard-failing
// (fail_rate >= hardFailThreshold) for >= emergencyEvictAfter wall-clock
// time, the system evicts it and promotes the first not-already-in-pool
// entry from cfg.EmergencyPromoteChain. ops doesn't have to be online —
// pool size stays at len(PoolMembers), no users left stranded on a dead
// node.
//
// Why only one auto behavior in manual mode: every auto pool change is a
// detection signal (re-HRW for ~1/K of terminals = "user IP changed" to
// upstream destinations). The 30-min sustained gate ensures the cost is
// only paid when the alternative — keeping a fully-dead node serving
// 1/K of users — is clearly worse.
//
// State separation:
//   - cfg.PoolMembers          = ops baseline from yaml (immutable per-deploy)
//   - s.yamlBaseline           = snapshot of PoolMembers when effective last in sync
//   - s.effectivePool          = current pool (= baseline ± emergency mutations)
//   - s.emergencyEvents        = audit log surfaced via /api/pool/state
//
// On startup: if cfg.PoolMembers != s.yamlBaseline, ops edited yaml →
// reset s.effectivePool to the new baseline (yaml change wins). Otherwise
// s.effectivePool persists across restarts so emergency state survives.

package nodescorer

import (
	"log/slog"
	"sort"
	"time"
)

// reconcileBaseline checks whether cfg.PoolMembers (yaml ground truth)
// has changed since the last save. If so, the operator has explicitly
// edited the pool — reset effectivePool to the new baseline and clear
// the recent event log (those events apply to the previous baseline).
//
// CALLER MUST HOLD s.mu.
func (s *Scorer) reconcileBaseline(yamlMembers []string) {
	baseline := append([]string(nil), yamlMembers...)
	sort.Strings(baseline)
	if stringSliceEqual(s.yamlBaseline, baseline) {
		// First-ever startup: yamlBaseline empty, but cfg.PoolMembers non-empty.
		// Adopt baseline; effectivePool also seeded from baseline.
		if len(s.effectivePool) == 0 && len(baseline) > 0 {
			s.effectivePool = append([]string(nil), baseline...)
			s.yamlBaseline = baseline
		}
		return
	}
	// yaml changed since last save → operator intent reset. Drop emergency
	// state and re-seed effectivePool from the new baseline.
	slog.Info("nodescorer: yaml pool_members changed since last save — resetting effective pool",
		"old_baseline", s.yamlBaseline, "new_baseline", baseline)
	s.yamlBaseline = baseline
	s.effectivePool = append([]string(nil), baseline...)
	// Drop events that referenced the old baseline; they're no longer
	// actionable — the new yaml represents a fresh ops decision.
	s.emergencyEvents = nil
}

// updateHardFailTimers walks each scored node and toggles its
// hardFailStart timestamp based on the latest fail_rate observation.
// Returns nodes whose hard-fail age has crossed the eviction threshold.
//
// CALLER MUST HOLD s.mu.
func (s *Scorer) updateHardFailTimers(nodes []NodeHealth, now time.Time) []string {
	var ripe []string
	for i := range nodes {
		st, ok := s.state[nodes[i].Name]
		if !ok {
			continue
		}
		if nodes[i].FailRate >= hardFailThreshold {
			if st.hardFailStart.IsZero() {
				st.hardFailStart = now
			}
		} else {
			st.hardFailStart = time.Time{}
		}
		// Only nodes currently in the effective pool are eligible for
		// emergency eviction; out-of-pool nodes can hard-fail freely
		// (they're not carrying traffic anyway).
		if !st.hardFailStart.IsZero() && now.Sub(st.hardFailStart) >= emergencyEvictAfter {
			if s.inEffectivePool(nodes[i].Name) {
				ripe = append(ripe, nodes[i].Name)
			}
		}
	}
	return ripe
}

// inEffectivePool reports whether the named tag is currently routing
// production traffic (∈ s.effectivePool).
//
// CALLER MUST HOLD s.mu.
func (s *Scorer) inEffectivePool(name string) bool {
	for _, m := range s.effectivePool {
		if m == name {
			return true
		}
	}
	return false
}

// applyEmergencyEvictions removes ripe-for-eviction members from the
// effective pool and promotes replacements from cfg.EmergencyPromoteChain
// in order. Returns true when at least one mutation occurred (caller
// triggers a hot-reload). The chain entries are filtered to those that
// (a) exist as candidates and (b) aren't already in the effective pool.
//
// CALLER MUST HOLD s.mu.
func (s *Scorer) applyEmergencyEvictions(ripe []string, candidates map[string]bool, now time.Time) bool {
	if len(ripe) == 0 {
		return false
	}
	mutated := false
	for _, evict := range ripe {
		// Remove from effective pool.
		next := make([]string, 0, len(s.effectivePool))
		for _, m := range s.effectivePool {
			if m != evict {
				next = append(next, m)
			}
		}
		if len(next) == len(s.effectivePool) {
			continue // already gone (shouldn't happen given inEffectivePool gate)
		}
		s.effectivePool = next
		s.recordEmergencyEvent(EmergencyEvent{
			At:     now,
			Type:   "evict",
			Node:   evict,
			Reason: "fail_rate >= " + floatStr(hardFailThreshold) + " sustained " + emergencyEvictAfter.String(),
		})
		slog.Error("nodescorer: emergency-evicting hard-failing pool member",
			"node", evict, "after", emergencyEvictAfter)

		// Promote next available chain entry. The just-evicted node is
		// excluded — it just hard-failed for 30min, re-promoting it from
		// the chain would defeat the purpose. Operator can clear the
		// emergency state and re-add it after diagnosing the issue.
		promoted := s.promoteFromChain(candidates, map[string]bool{evict: true})
		if promoted == "" {
			s.recordEmergencyEvent(EmergencyEvent{
				At:     now,
				Type:   "exhausted",
				Reason: "no chain entry available; pool size shrunk",
			})
			slog.Error("nodescorer: emergency_promote_chain exhausted — pool shrunk",
				"effective_size", len(s.effectivePool))
		} else {
			s.recordEmergencyEvent(EmergencyEvent{
				At:     now,
				Type:   "promote",
				Node:   promoted,
				Reason: "auto-promote from emergency_promote_chain (replacing " + evict + ")",
			})
			slog.Info("nodescorer: emergency-promoted from chain",
				"node", promoted, "replacing", evict)
		}
		mutated = true
	}
	return mutated
}

// promoteFromChain finds the first cfg.EmergencyPromoteChain entry that
// (a) is in the candidate set (i.e. exists in the current outbounds),
// (b) isn't already in s.effectivePool, and (c) isn't in skip (used to
// keep just-evicted nodes from being immediately re-promoted), appends
// it, and returns the tag. Empty string when no eligible chain entry
// remains.
//
// CALLER MUST HOLD s.mu.
func (s *Scorer) promoteFromChain(candidates, skip map[string]bool) string {
	inPool := make(map[string]bool, len(s.effectivePool))
	for _, m := range s.effectivePool {
		inPool[m] = true
	}
	for _, cand := range s.cfg.EmergencyPromoteChain {
		if !candidates[cand] {
			continue
		}
		if inPool[cand] {
			continue
		}
		if skip[cand] {
			continue
		}
		s.effectivePool = append(s.effectivePool, cand)
		return cand
	}
	return ""
}

// recordEmergencyEvent appends to the in-memory event log, capped at
// emergencyEventLogMax. CALLER MUST HOLD s.mu. Fires the eventHook
// callback (if set) so the notification subsystem can forward the
// event to Lark + the JSONL log without the scorer importing the
// notify package directly.
func (s *Scorer) recordEmergencyEvent(ev EmergencyEvent) {
	s.emergencyEvents = append(s.emergencyEvents, ev)
	if len(s.emergencyEvents) > emergencyEventLogMax {
		s.emergencyEvents = s.emergencyEvents[len(s.emergencyEvents)-emergencyEventLogMax:]
	}
	if s.eventHook != nil {
		// Run hook on a goroutine so it can't deadlock against s.mu —
		// recordEmergencyEvent is always called under the scorer lock,
		// and the hook may want to call back into APIs that take the
		// lock (e.g. GetSnapshot for richer notification payloads).
		go s.eventHook(ev)
	}
}

// SetEventHook registers a callback to fire on every recorded
// EmergencyEvent. main.go calls this once at startup with the
// notify.Notifier's emit shim. Safe to call before or after Run().
func (s *Scorer) SetEventHook(h func(EmergencyEvent)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventHook = h
}

// PoolState is the externalized view of the manual-mode pool state,
// surfaced via GET /api/pool/state. Lets ops see exactly what's in the
// routing pool right now, what the yaml says it should be, and the
// recent emergency events that drove any divergence.
type PoolState struct {
	Mode            string           `json:"mode"`             // "manual" | "auto" | other
	YamlBaseline    []string         `json:"yaml_baseline"`    // cfg.PoolMembers (sorted)
	EffectivePool   []string         `json:"effective_pool"`   // currently routing
	Diverged        bool             `json:"diverged"`         // effective != baseline
	PromoteChain    []string         `json:"promote_chain"`    // cfg.EmergencyPromoteChain
	EmergencyEvents []EmergencyEvent `json:"emergency_events"` // recent log, oldest-first
}

// GetPoolState returns the current externalized pool state. Safe to
// call concurrently — takes a read lock.
func (s *Scorer) GetPoolState() PoolState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	baseline := append([]string(nil), s.cfg.PoolMembers...)
	sort.Strings(baseline)
	effective := append([]string(nil), s.effectivePool...)
	sort.Strings(effective)
	return PoolState{
		Mode:            s.cfg.PoolMode,
		YamlBaseline:    baseline,
		EffectivePool:   effective,
		Diverged:        !stringSliceEqual(baseline, effective),
		PromoteChain:    append([]string(nil), s.cfg.EmergencyPromoteChain...),
		EmergencyEvents: append([]EmergencyEvent(nil), s.emergencyEvents...),
	}
}

// ClearEmergency reverts the effective pool to the yaml baseline,
// dropping all emergency mutations + the event log. The next scoring
// round detects the change and triggers exactly one hot-reload to push
// the reverted pool to mihomo. ops calls this after diagnosing why a
// node hard-failed and either fixing the upstream issue or accepting
// that the node is gone (and editing yaml to reflect the new baseline).
//
// Idempotent: when there's nothing to clear (effective already equals
// baseline AND no node is currently hard-failing), returns without
// recording an event or resetting timers — calling clear-emergency in
// a steady state should be a true no-op, not a noise generator.
func (s *Scorer) ClearEmergency() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cfg.PoolMembers) == 0 {
		return
	}
	baseline := append([]string(nil), s.cfg.PoolMembers...)
	sort.Strings(baseline)

	// Detect actual divergence: effective != baseline OR any node has an
	// active hard-fail timer ticking. If neither, this call is a no-op.
	effectiveSorted := append([]string(nil), s.effectivePool...)
	sort.Strings(effectiveSorted)
	hasActiveTimer := false
	for _, st := range s.state {
		if !st.hardFailStart.IsZero() {
			hasActiveTimer = true
			break
		}
	}
	if stringSliceEqual(effectiveSorted, baseline) && !hasActiveTimer {
		return
	}

	s.effectivePool = append([]string(nil), s.cfg.PoolMembers...)
	s.yamlBaseline = baseline
	s.recordEmergencyEvent(EmergencyEvent{
		At:     time.Now(),
		Type:   "clear",
		Reason: "operator cleared emergency state via /api/pool/clear-emergency",
	})
	// Reset hard-fail timers so a still-flapping node gets a fresh
	// 30-min grace period before re-eviction.
	for _, st := range s.state {
		st.hardFailStart = time.Time{}
	}
	s.saveStateLocked()
}

// stringSliceEqual reports whether two sorted []string are equal.
func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// floatStr formats a float to 2 decimals — used for log reasons.
func floatStr(f float64) string {
	// Avoid pulling in fmt for one call; manual format is enough for
	// values 0.0-1.0 with two decimals.
	t := int(f * 100)
	whole := t / 100
	frac := t % 100
	out := []byte{byte('0' + whole), '.', byte('0' + frac/10), byte('0' + frac%10)}
	return string(out)
}
