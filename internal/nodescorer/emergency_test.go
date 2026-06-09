package nodescorer

import (
	"testing"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
	"github.com/leap-gateway/leap-gateway/internal/subscribe"
)

// emergencyCfg builds a manual-mode config for the tests.
func emergencyCfg(members, chain []string) config.NodeQualifyConfig {
	c := defaultCfg()
	c.PoolMode = "manual"
	c.PoolMembers = append([]string(nil), members...)
	c.EmergencyPromoteChain = append([]string(nil), chain...)
	return c
}

// addStateNode is the lightweight test helper: ensures s.state[tag] exists.
func addStateNode(s *Scorer, tag string) *nodeState {
	if s.state == nil {
		s.state = map[string]*nodeState{}
	}
	if s.state[tag] == nil {
		s.state[tag] = &nodeState{}
	}
	return s.state[tag]
}

// TestUpdateHardFailTimers covers timer set / clear / ripeness check.
func TestUpdateHardFailTimers(t *testing.T) {
	s := newTestScorer(emergencyCfg([]string{"a", "b"}, nil))
	s.effectivePool = []string{"a", "b"}
	addStateNode(s, "a")
	addStateNode(s, "b")

	now := time.Now()

	// Round 1: a is hard-failing, b is healthy. Timer should arm on a.
	ripe := s.updateHardFailTimers([]NodeHealth{
		{Name: "a", FailRate: 1.0, ProbeCount: 10},
		{Name: "b", FailRate: 0.0, ProbeCount: 10},
	}, now)
	if len(ripe) != 0 {
		t.Errorf("round 1 ripe = %v, want none yet (just-armed)", ripe)
	}
	if s.state["a"].hardFailStart.IsZero() {
		t.Error("a's hardFailStart should be set")
	}
	if !s.state["b"].hardFailStart.IsZero() {
		t.Error("b's hardFailStart should remain zero")
	}

	// Round 2 (29 minutes later): a still hard-failing → not ripe yet.
	round2 := now.Add(29 * time.Minute)
	ripe = s.updateHardFailTimers([]NodeHealth{
		{Name: "a", FailRate: 1.0, ProbeCount: 10},
		{Name: "b", FailRate: 0.0, ProbeCount: 10},
	}, round2)
	if len(ripe) != 0 {
		t.Errorf("round 2 ripe = %v, want none (29min < 30min)", ripe)
	}

	// Round 3 (31 minutes later): a should now be ripe.
	round3 := now.Add(31 * time.Minute)
	ripe = s.updateHardFailTimers([]NodeHealth{
		{Name: "a", FailRate: 1.0, ProbeCount: 10},
		{Name: "b", FailRate: 0.0, ProbeCount: 10},
	}, round3)
	if len(ripe) != 1 || ripe[0] != "a" {
		t.Errorf("round 3 ripe = %v, want [a]", ripe)
	}

	// Round 4: a recovers. Timer cleared, no longer ripe.
	round4 := round3.Add(1 * time.Minute)
	ripe = s.updateHardFailTimers([]NodeHealth{
		{Name: "a", FailRate: 0.1, ProbeCount: 10},
		{Name: "b", FailRate: 0.0, ProbeCount: 10},
	}, round4)
	if len(ripe) != 0 {
		t.Errorf("round 4 ripe = %v, want [] after recovery", ripe)
	}
	if !s.state["a"].hardFailStart.IsZero() {
		t.Error("a's hardFailStart should be cleared after recovery")
	}
}

// TestApplyEmergencyEvictions covers the evict + promote loop.
func TestApplyEmergencyEvictions(t *testing.T) {
	s := newTestScorer(emergencyCfg(
		[]string{"a", "b", "c"},
		[]string{"x", "y", "z"},
	))
	s.effectivePool = []string{"a", "b", "c"}
	candidates := map[string]bool{
		"a": true, "b": true, "c": true, "x": true, "y": true, "z": true,
	}

	// Evict "a". Should promote "x" (first chain entry, in candidates,
	// not in pool).
	mutated := s.applyEmergencyEvictions([]string{"a"}, candidates, time.Now())
	if !mutated {
		t.Fatal("expected mutation")
	}
	if !contains(s.effectivePool, "x") || contains(s.effectivePool, "a") {
		t.Errorf("after evict-a/promote-x: effectivePool = %v", s.effectivePool)
	}
	if len(s.effectivePool) != 3 {
		t.Errorf("pool size after evict+promote = %d, want 3", len(s.effectivePool))
	}

	// Two emergency events: evict + promote.
	if len(s.emergencyEvents) != 2 {
		t.Errorf("emergencyEvents = %d, want 2 (evict, promote)", len(s.emergencyEvents))
	}
}

// TestPromoteFromChainSkipsAlreadyInPool: chain entries already in pool
// are skipped over. Chain ["a", "x"] with pool ["a"] should promote "x".
func TestPromoteFromChainSkipsAlreadyInPool(t *testing.T) {
	s := newTestScorer(emergencyCfg([]string{"a"}, []string{"a", "x"}))
	s.effectivePool = []string{"a"}
	cands := map[string]bool{"a": true, "x": true}

	got := s.promoteFromChain(cands, nil)
	if got != "x" {
		t.Errorf("promoteFromChain = %q, want %q (a is already in pool)", got, "x")
	}
}

// TestPromoteFromChainExhausted: when no chain entry is eligible (all
// already in pool, or none in candidates), returns empty and pool shrinks.
func TestPromoteFromChainExhausted(t *testing.T) {
	s := newTestScorer(emergencyCfg([]string{"a", "b"}, []string{"a"}))
	s.effectivePool = []string{"a", "b"}
	cands := map[string]bool{"a": true, "b": true}

	mutated := s.applyEmergencyEvictions([]string{"a"}, cands, time.Now())
	if !mutated {
		t.Fatal("expected mutation (evict happened even if no promote)")
	}
	if contains(s.effectivePool, "a") || !contains(s.effectivePool, "b") {
		t.Errorf("effective after exhaust: %v", s.effectivePool)
	}
	if len(s.effectivePool) != 1 {
		t.Errorf("pool size after exhausted promote = %d, want 1 (shrunk)", len(s.effectivePool))
	}
	// Should have evict + exhausted events.
	hasExhausted := false
	for _, ev := range s.emergencyEvents {
		if ev.Type == "exhausted" {
			hasExhausted = true
		}
	}
	if !hasExhausted {
		t.Error("expected an 'exhausted' event when chain has no eligible entry")
	}
}

// TestReconcileBaselineYamlChange: when cfg.PoolMembers differs from the
// stored yamlBaseline, reset effective to the new baseline.
func TestReconcileBaselineYamlChange(t *testing.T) {
	s := newTestScorer(emergencyCfg([]string{"a", "b", "x"}, nil))
	// Simulate prior state: effective drifted via emergency to ["a", "b", "x"]
	// (b replaced y). yamlBaseline was the old set ["a", "b", "y"].
	s.yamlBaseline = []string{"a", "b", "y"}
	s.effectivePool = []string{"a", "b", "x"}
	s.emergencyEvents = []EmergencyEvent{{Type: "evict"}}

	// Now ops edits yaml to [a, b, x] — reconcile should adopt new
	// baseline AND drop emergency state (because the new baseline
	// matches what was effective).
	s.reconcileBaseline([]string{"a", "b", "x"})
	if !contains(s.yamlBaseline, "x") {
		t.Errorf("yamlBaseline after reconcile: %v", s.yamlBaseline)
	}
	// effective should now match new baseline (sorted slice equality).
	expected := []string{"a", "b", "x"}
	if !setEq(s.effectivePool, expected) {
		t.Errorf("effective after reconcile: %v, want %v", s.effectivePool, expected)
	}
	if len(s.emergencyEvents) != 0 {
		t.Errorf("emergencyEvents should be cleared on yaml change, got %d", len(s.emergencyEvents))
	}
}

// TestReconcileBaselineFirstStartup: yamlBaseline empty, cfg.PoolMembers
// non-empty → adopt baseline + seed effective.
func TestReconcileBaselineFirstStartup(t *testing.T) {
	s := newTestScorer(emergencyCfg([]string{"a", "b"}, nil))
	s.reconcileBaseline([]string{"a", "b"})
	if !setEq(s.yamlBaseline, []string{"a", "b"}) {
		t.Errorf("baseline first startup: %v", s.yamlBaseline)
	}
	if !setEq(s.effectivePool, []string{"a", "b"}) {
		t.Errorf("effective first startup: %v", s.effectivePool)
	}
}

// TestClearEmergencyRevertsAndResetsTimers verifies the operator's
// "I've handled it" path: ClearEmergency reverts effective to baseline +
// resets all hard-fail timers + records a clear event.
func TestClearEmergencyRevertsAndResetsTimers(t *testing.T) {
	s := newTestScorer(emergencyCfg([]string{"a", "b"}, nil))
	s.renderer = &emergencyTestRenderer{path: t.TempDir() + "/cfg.yaml"}
	s.yamlBaseline = []string{"a", "b"}
	s.effectivePool = []string{"a", "x"} // drifted via emergency
	addStateNode(s, "a").hardFailStart = time.Now().Add(-10 * time.Minute)
	addStateNode(s, "b")

	s.ClearEmergency()
	if !setEq(s.effectivePool, []string{"a", "b"}) {
		t.Errorf("effective after clear: %v, want [a b]", s.effectivePool)
	}
	if !s.state["a"].hardFailStart.IsZero() {
		t.Error("a's hardFailStart should be zero after clear")
	}
	hasClear := false
	for _, ev := range s.emergencyEvents {
		if ev.Type == "clear" {
			hasClear = true
		}
	}
	if !hasClear {
		t.Error("expected 'clear' event")
	}
}

// TestClearEmergencyIdempotent: when effective == baseline AND no
// hard-fail timer is active, ClearEmergency must be a true no-op —
// no event recorded, no state mutation. This was a 2026-06-09 fix
// after a test run accidentally wrote a "clear" event into a clean
// state, which is operationally noise.
func TestClearEmergencyIdempotent(t *testing.T) {
	s := newTestScorer(emergencyCfg([]string{"a", "b"}, nil))
	s.renderer = &emergencyTestRenderer{path: t.TempDir() + "/cfg.yaml"}
	s.yamlBaseline = []string{"a", "b"}
	s.effectivePool = []string{"a", "b"} // already in sync
	addStateNode(s, "a")                   // no hardFailStart
	addStateNode(s, "b")
	priorEvents := len(s.emergencyEvents)

	s.ClearEmergency()

	if len(s.emergencyEvents) != priorEvents {
		t.Errorf("expected no new event when state was already clean, got %d new",
			len(s.emergencyEvents)-priorEvents)
	}
}

// TestClearEmergencyNotIdempotentWhenTimerActive: when a hard-fail timer
// is ticking, ClearEmergency must reset it (so the node gets a fresh
// 30-min grace period) AND record a clear event so the operator sees
// that timers were touched. This is the explicit "I'm canceling the
// auto-evict path for this node" signal.
func TestClearEmergencyNotIdempotentWhenTimerActive(t *testing.T) {
	s := newTestScorer(emergencyCfg([]string{"a", "b"}, nil))
	s.renderer = &emergencyTestRenderer{path: t.TempDir() + "/cfg.yaml"}
	s.yamlBaseline = []string{"a", "b"}
	s.effectivePool = []string{"a", "b"} // in sync...
	st := addStateNode(s, "a")
	st.hardFailStart = time.Now().Add(-10 * time.Minute) // ...but a timer is running
	addStateNode(s, "b")

	s.ClearEmergency()

	if !s.state["a"].hardFailStart.IsZero() {
		t.Error("hardFailStart should be cleared")
	}
	hasClear := false
	for _, ev := range s.emergencyEvents {
		if ev.Type == "clear" {
			hasClear = true
		}
	}
	if !hasClear {
		t.Error("expected a clear event when timer was active")
	}
}

// emergencyTestRenderer is a minimal Renderer for tests that need
// Path() (e.g. ClearEmergency triggers saveStateLocked which needs
// renderer.Path()).
type emergencyTestRenderer struct{ path string }

func (r *emergencyTestRenderer) RenderWithQualifiedNodes(_ []subscribe.Outbound, _ []string) ([]byte, error) {
	return nil, nil
}

func (r *emergencyTestRenderer) RenderWithPools(_ []subscribe.Outbound, _, _ []string, _ map[string][]string) ([]byte, error) {
	return nil, nil
}

func (r *emergencyTestRenderer) Path() string { return r.path }

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func setEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	am := map[string]bool{}
	for _, x := range a {
		am[x] = true
	}
	for _, x := range b {
		if !am[x] {
			return false
		}
	}
	return true
}
