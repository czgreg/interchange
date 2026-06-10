// transitions.go — pool composition audit log + rollback machinery.
//
// Every time the runtime routing pool (s.poolSet in auto mode or
// s.effectivePool in manual mode) gains or loses a member, recordTransition
// captures the diff with timestamp + reason + source. The list is
// persisted in the state file (v3 schema) so audit history survives
// restarts.
//
// Rollback consumes the last N transitions in reverse: each step undoes
// the diff (re-adds removed members, removes added members) AND puts
// any node that was just-promoted into a quarantine set. The K-gating
// auto-mode loop refuses to re-add a quarantined node until its
// quarantine-until timestamp passes, so a rollback isn't immediately
// reversed by the next scoring round.

package nodescorer

import (
	"sort"
	"time"
)

// PoolTransition records one mutation of the runtime routing pool.
// Surfaced via /api/pool/transitions for ops audit + drives /api/pool/
// rollback's reverse-replay logic.
type PoolTransition struct {
	At         time.Time `json:"at"`
	Type       string    `json:"type"`         // "auto_swap" | "manual_swap" | "emergency_evict" | "emergency_promote" | "rollback" | "init"
	Added      []string  `json:"added,omitempty"`
	Removed    []string  `json:"removed,omitempty"`
	PoolBefore []string  `json:"pool_before"`
	PoolAfter  []string  `json:"pool_after"`
	Reason     string    `json:"reason,omitempty"`
	Source     string    `json:"source"` // "scorer" | "operator_api" | "operator_yaml"
}

const (
	// transitionLogMax caps in-memory + persisted transitions. Each
	// transition is ~150-300 bytes; 200 ≈ 50 KB. Older transitions are
	// dropped silently — operators querying /api/pool/transitions get
	// at most this many entries (limit query param caps below this).
	transitionLogMax = 200

	// rollbackQuarantineDefault is how long a just-rolled-back node is
	// blocked from re-entering the pool. 1h gives ops time to investigate
	// + extend quarantine or change config. Configurable via
	// rollback request body.
	rollbackQuarantineDefault = 1 * time.Hour

	// rollbackQuarantineMax caps the operator's per-request quarantine.
	// 7 days — beyond that the operator should edit yaml to permanently
	// exclude the node, not rely on quarantine.
	rollbackQuarantineMax = 7 * 24 * time.Hour
)

// recordTransitionLocked appends a transition to the audit log, capping
// at transitionLogMax. Fires transitionHook (if set) so the notify
// subsystem can surface pool drifts to Lark. CALLER MUST HOLD s.mu.
func (s *Scorer) recordTransitionLocked(t PoolTransition) {
	if t.At.IsZero() {
		t.At = time.Now()
	}
	// Sort in/out for deterministic output (test stability + diff ergonomic).
	sort.Strings(t.Added)
	sort.Strings(t.Removed)
	s.transitions = append(s.transitions, t)
	if len(s.transitions) > transitionLogMax {
		s.transitions = s.transitions[len(s.transitions)-transitionLogMax:]
	}
	if s.transitionHook != nil {
		// Run on a goroutine — recordTransitionLocked is always called
		// under s.mu and the hook may take the lock back to enrich the
		// notification with snapshot data.
		go s.transitionHook(t)
	}
}

// SetTransitionHook registers a callback fired on every recorded
// PoolTransition. main.go wires this to notify.Notifier.Emit with a
// formatter that turns added/removed diffs into a Lark message body.
func (s *Scorer) SetTransitionHook(h func(PoolTransition)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transitionHook = h
}

// diffPools computes the symmetric difference: added = b\a, removed = a\b.
// Both inputs assumed sorted by caller for reproducibility.
func diffPools(a, b []string) (added, removed []string) {
	inA := make(map[string]bool, len(a))
	inB := make(map[string]bool, len(b))
	for _, x := range a {
		inA[x] = true
	}
	for _, x := range b {
		inB[x] = true
	}
	for _, x := range b {
		if !inA[x] {
			added = append(added, x)
		}
	}
	for _, x := range a {
		if !inB[x] {
			removed = append(removed, x)
		}
	}
	return
}

// inQuarantine reports whether the named node is currently blocked from
// being added to the pool by a recent rollback. CALLER MUST HOLD s.mu.
func (s *Scorer) inQuarantine(node string, now time.Time) bool {
	until, ok := s.rollbackQuarantine[node]
	if !ok {
		return false
	}
	if now.Before(until) {
		return true
	}
	// Expired — clean up so the map doesn't grow unbounded.
	delete(s.rollbackQuarantine, node)
	return false
}

// GetTransitions returns up to limit most-recent transitions (newest
// last). Safe to call concurrently.
func (s *Scorer) GetTransitions(limit int) []PoolTransition {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > len(s.transitions) {
		limit = len(s.transitions)
	}
	out := make([]PoolTransition, limit)
	copy(out, s.transitions[len(s.transitions)-limit:])
	return out
}

// GetQuarantine returns the current quarantine map (node → until-time).
// Used by /api/pool/transitions to surface "these nodes are currently
// blocked from re-entry" alongside the audit log.
func (s *Scorer) GetQuarantine() map[string]time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]time.Time, len(s.rollbackQuarantine))
	now := time.Now()
	for k, t := range s.rollbackQuarantine {
		if now.Before(t) {
			out[k] = t
		}
	}
	return out
}

// PoolStability captures how much the pool has churned in a recent
// window. Surfaced via /api/status so ops can answer "is the system
// thrashing?" without scrolling through transition logs.
type PoolStability struct {
	WindowHours    int       `json:"window_hours"`     // 24
	SwapCount      int       `json:"swap_count"`       // total non-rollback transitions in window
	RollbackCount  int       `json:"rollback_count"`   // operator rollback events
	UniqueNodesIn  int       `json:"unique_nodes_in"`  // distinct nodes that were Added across all transitions
	UniqueNodesOut int       `json:"unique_nodes_out"` // distinct nodes that were Removed
	OldestAt       time.Time `json:"oldest_at,omitempty"`
}

// GetStability returns the 24h pool stability summary. Cheap — walks
// the in-memory transitions slice once.
func (s *Scorer) GetStability() PoolStability {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cutoff := time.Now().Add(-24 * time.Hour)
	out := PoolStability{WindowHours: 24}
	in, outNodes := map[string]bool{}, map[string]bool{}
	for _, t := range s.transitions {
		if t.At.Before(cutoff) {
			continue
		}
		if out.OldestAt.IsZero() || t.At.Before(out.OldestAt) {
			out.OldestAt = t.At
		}
		switch t.Type {
		case "rollback":
			out.RollbackCount++
		default:
			out.SwapCount++
		}
		for _, n := range t.Added {
			in[n] = true
		}
		for _, n := range t.Removed {
			outNodes[n] = true
		}
	}
	out.UniqueNodesIn = len(in)
	out.UniqueNodesOut = len(outNodes)
	return out
}

// RollbackResult is returned by Rollback to summarize the operation.
type RollbackResult struct {
	StepsRequested int       `json:"steps_requested"`
	StepsApplied   int       `json:"steps_applied"`
	Reverted       []string  `json:"reverted_transition_types,omitempty"`
	NewPool        []string  `json:"new_pool"`
	Quarantined    []string  `json:"quarantined,omitempty"`
	QuarantineUntil time.Time `json:"quarantine_until,omitempty"`
}

// Rollback undoes the most-recent N transitions (auto K-gating swaps,
// emergency evictions/promotions). Each step un-does one transition by
// reversing its diff: nodes the transition Added are removed; nodes it
// Removed are re-added. The rolled-back nodes (i.e. nodes the system
// had recently put in pool) are placed in quarantine so the next
// scoring round doesn't re-promote them — giving ops time to investigate
// + decide whether to extend quarantine, edit yaml, or accept and clear.
//
// Returns the RollbackResult with the actual count applied (may be less
// than steps if the audit log has fewer entries) and the post-rollback
// pool composition.
//
// Only applies to auto-mode poolSet (s.poolSet). In manual mode, ops
// should use /api/pool/clear-emergency or edit yaml directly. Returns
// an error in manual mode.
func (s *Scorer) Rollback(steps int, quarantineDuration time.Duration) (RollbackResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.PoolMode == "manual" {
		return RollbackResult{}, errSimpleMode("rollback only applies in pool_mode=auto; use /api/pool/clear-emergency in manual mode")
	}
	if steps <= 0 {
		steps = 1
	}
	if quarantineDuration <= 0 {
		quarantineDuration = rollbackQuarantineDefault
	}
	if quarantineDuration > rollbackQuarantineMax {
		quarantineDuration = rollbackQuarantineMax
	}

	// Find rollback-eligible transitions (skip prior 'rollback' entries —
	// rolling back a rollback is too confusing; ops can re-trigger via
	// scorer's normal path).
	idx := len(s.transitions) - 1
	applied := 0
	revertedTypes := []string{}
	quarantined := map[string]bool{}
	now := time.Now()
	until := now.Add(quarantineDuration)

	currentPool := setToSortedSlice(s.poolSet)

	for applied < steps && idx >= 0 {
		t := s.transitions[idx]
		idx--
		if t.Type == "rollback" {
			continue
		}
		// Apply the reverse diff.
		newPool := make(map[string]bool, len(currentPool))
		for _, m := range currentPool {
			newPool[m] = true
		}
		// What this transition added → we now remove (and quarantine)
		for _, name := range t.Added {
			delete(newPool, name)
			s.rollbackQuarantine[name] = until
			quarantined[name] = true
		}
		// What this transition removed → we now restore
		for _, name := range t.Removed {
			newPool[name] = true
		}
		currentPool = setToSortedSlice(boolMapToSet(newPool))
		applied++
		revertedTypes = append(revertedTypes, t.Type)
	}

	if applied == 0 {
		return RollbackResult{
			StepsRequested: steps,
			StepsApplied:   0,
			NewPool:        currentPool,
		}, nil
	}

	// Commit: rebuild s.poolSet, record a rollback transition, save state.
	beforeSet := setToSortedSlice(s.poolSet)
	newSet := boolMapToSet(stringSliceToMap(currentPool))
	s.poolSet = newSet
	added, removed := diffPools(beforeSet, currentPool)
	s.recordTransitionLocked(PoolTransition{
		At:         now,
		Type:       "rollback",
		Added:      added,
		Removed:    removed,
		PoolBefore: beforeSet,
		PoolAfter:  currentPool,
		Reason:     "operator rollback (" + intToStr(applied) + " step(s))",
		Source:     "operator_api",
	})
	s.saveStateLocked()

	qList := make([]string, 0, len(quarantined))
	for k := range quarantined {
		qList = append(qList, k)
	}
	sort.Strings(qList)
	return RollbackResult{
		StepsRequested:  steps,
		StepsApplied:    applied,
		Reverted:        revertedTypes,
		NewPool:         currentPool,
		Quarantined:     qList,
		QuarantineUntil: until,
	}, nil
}

// boolMapToSet converts a string→bool map to map[string]bool with only
// true keys. (Used because s.poolSet is map[string]bool and we sometimes
// build it from a different shape.)
func boolMapToSet(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k, v := range in {
		if v {
			out[k] = true
		}
	}
	return out
}

func stringSliceToMap(s []string) map[string]bool {
	out := make(map[string]bool, len(s))
	for _, x := range s {
		out[x] = true
	}
	return out
}

// errSimpleMode is a thin error type used to signal mode-specific errors
// (rollback/auto vs manual). Keeps the API handler's error mapping clean.
type errSimpleMode string

func (e errSimpleMode) Error() string { return string(e) }

func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
