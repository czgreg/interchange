package nodescorer

import (
	"math"
	"testing"
	"time"
)

// TestEWMAUpdate covers the basic time-aware decay: an observation at
// halfLife elapsed gets exactly 50% weight; back-to-back observations
// at zero elapsed don't move the value.
func TestEWMAUpdate(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var p ewmaPoint

	// First call seeds the EWMA at the observation.
	p.update(100, now, 4*time.Hour)
	if p.Value != 100 {
		t.Errorf("seed: value=%v, want 100", p.Value)
	}

	// One half-life later, observation 200 → expected (1-0.5)*100 + 0.5*200 = 150.
	now2 := now.Add(4 * time.Hour)
	p.update(200, now2, 4*time.Hour)
	if math.Abs(p.Value-150) > 0.01 {
		t.Errorf("after 1 half-life: value=%v, want ~150", p.Value)
	}

	// Two more half-lives later (3 total since seed), observation 400.
	// Decay coefficient (1-2^-2) = 0.75 against the 150 we have.
	// new = 0.75*400 + 0.25*150 = 300 + 37.5 = 337.5
	now3 := now2.Add(8 * time.Hour)
	p.update(400, now3, 4*time.Hour)
	if math.Abs(p.Value-337.5) > 0.01 {
		t.Errorf("after 3 half-lives total: value=%v, want ~337.5", p.Value)
	}

	// Out-of-order update: just refreshes time, no math.
	p.update(999, now2, 4*time.Hour) // backdated
	if math.Abs(p.Value-337.5) > 0.01 {
		t.Errorf("backdated update should not change value, got %v", p.Value)
	}
}

// TestUpdateNodeEWMA: per-node tracking + FirstSeenAt set on first call.
func TestUpdateNodeEWMA(t *testing.T) {
	s := newTestScorer(emergencyCfg(nil, nil))
	s.cfg.PoolMode = "auto"
	s.ewma = map[string]*nodeEWMA{}

	now := time.Now()
	s.updateNodeEWMA("a", 250, now)

	e := s.ewma["a"]
	if e == nil {
		t.Fatal("ewma[a] should exist after update")
	}
	if e.FirstSeenAt.IsZero() {
		t.Error("FirstSeenAt should be set on first observation")
	}
	if e.Short.Value != 250 || e.Long.Value != 250 {
		t.Errorf("first observation should seed both: short=%v long=%v", e.Short.Value, e.Long.Value)
	}

	// Second update later — both windows should respond, short more aggressively.
	s.updateNodeEWMA("a", 1000, now.Add(2*time.Hour))
	if e.Short.Value <= e.Long.Value {
		t.Errorf("short EWMA should track upward faster than long: short=%v long=%v",
			e.Short.Value, e.Long.Value)
	}
}

// TestInTrial: nodes with <24h history are in trial; older ones aren't.
func TestInTrial(t *testing.T) {
	s := newTestScorer(emergencyCfg(nil, nil))
	s.cfg.PoolMode = "auto"
	now := time.Now()

	// Brand new — never seen.
	if !s.inTrial("never-seen", now) {
		t.Error("a node we've never seen should be in trial")
	}

	// Newly added: FirstSeenAt = now.
	s.updateNodeEWMA("fresh", 250, now)
	if !s.inTrial("fresh", now) {
		t.Error("freshly observed node should be in trial")
	}
	if !s.inTrial("fresh", now.Add(23*time.Hour)) {
		t.Error("23h after first-seen should still be in trial")
	}
	if s.inTrial("fresh", now.Add(25*time.Hour)) {
		t.Error("25h after first-seen should NOT be in trial")
	}
}

// TestPoolStability: counts non-rollback transitions in 24h window.
func TestPoolStability(t *testing.T) {
	s := newTestScorer(emergencyCfg(nil, nil))
	s.cfg.PoolMode = "auto"
	now := time.Now()
	s.transitions = []PoolTransition{
		{At: now.Add(-30 * time.Hour), Type: "auto_swap", Added: []string{"a"}, Removed: []string{"b"}}, // outside window
		{At: now.Add(-12 * time.Hour), Type: "auto_swap", Added: []string{"c"}, Removed: []string{"d"}},
		{At: now.Add(-6 * time.Hour), Type: "rollback", Added: []string{"d"}, Removed: []string{"c"}},
		{At: now.Add(-1 * time.Hour), Type: "auto_swap", Added: []string{"e"}, Removed: []string{"f"}},
	}
	st := s.GetStability()
	if st.WindowHours != 24 {
		t.Errorf("window=%d, want 24", st.WindowHours)
	}
	if st.SwapCount != 2 {
		t.Errorf("SwapCount=%d, want 2 (one was rollback, one was outside window)", st.SwapCount)
	}
	if st.RollbackCount != 1 {
		t.Errorf("RollbackCount=%d, want 1", st.RollbackCount)
	}
	wantIn := map[string]bool{"c": true, "d": true, "e": true} // 3 unique nodes added across all transitions in window
	if st.UniqueNodesIn != len(wantIn) {
		t.Errorf("UniqueNodesIn=%d, want %d", st.UniqueNodesIn, len(wantIn))
	}
}
