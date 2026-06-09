package nodescorer

import (
	"testing"
	"time"
)

// TestDiffPools is the building block for the audit log + rollback —
// must correctly compute symmetric differences.
func TestDiffPools(t *testing.T) {
	cases := []struct {
		a, b           []string
		wantA, wantR   []string
	}{
		{
			a: []string{"a", "b", "c"}, b: []string{"a", "b", "c"},
			wantA: nil, wantR: nil,
		},
		{
			a: []string{"a", "b"}, b: []string{"a", "b", "c"},
			wantA: []string{"c"}, wantR: nil,
		},
		{
			a: []string{"a", "b", "c"}, b: []string{"a", "b"},
			wantA: nil, wantR: []string{"c"},
		},
		{
			a: []string{"a", "b"}, b: []string{"b", "c"},
			wantA: []string{"c"}, wantR: []string{"a"},
		},
	}
	for _, tc := range cases {
		gotA, gotR := diffPools(tc.a, tc.b)
		if !setEq(gotA, tc.wantA) || !setEq(gotR, tc.wantR) {
			t.Errorf("diffPools(%v, %v) = added %v removed %v, want added %v removed %v",
				tc.a, tc.b, gotA, gotR, tc.wantA, tc.wantR)
		}
	}
}

// TestRollbackUndoesLastTransition: the basic Rollback flow.
//   1. simulate a swap (a → x via auto K-gating)
//   2. rollback → pool goes back to having a, x removed
//   3. quarantine has x with until-time in the future
func TestRollbackUndoesLastTransition(t *testing.T) {
	s := newTestScorer(emergencyCfg(nil, nil))
	s.cfg.PoolMode = "auto"
	s.renderer = &emergencyTestRenderer{path: t.TempDir() + "/cfg.yaml"}
	s.poolSet = map[string]bool{"a": true, "b": true, "c": true}

	now := time.Now()
	s.recordTransitionLocked(PoolTransition{
		At:         now.Add(-1 * time.Minute),
		Type:       "auto_swap",
		Added:      []string{"x"},
		Removed:    []string{"a"},
		PoolBefore: []string{"a", "b", "c"},
		PoolAfter:  []string{"b", "c", "x"},
		Source:     "scorer",
	})
	// also reflect in poolSet (real flow: swap just happened)
	s.poolSet = map[string]bool{"b": true, "c": true, "x": true}

	res, err := s.Rollback(1, 1*time.Hour)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if res.StepsApplied != 1 {
		t.Errorf("StepsApplied = %d, want 1", res.StepsApplied)
	}
	wantPool := map[string]bool{"a": true, "b": true, "c": true}
	for k := range wantPool {
		if !s.poolSet[k] {
			t.Errorf("post-rollback poolSet missing %q (got %v)", k, s.poolSet)
		}
	}
	if s.poolSet["x"] {
		t.Errorf("x should be removed from poolSet after rollback (got %v)", s.poolSet)
	}
	// Quarantine should contain x with until-time ~1h in future.
	q := s.GetQuarantine()
	if _, ok := q["x"]; !ok {
		t.Errorf("x should be quarantined; got %v", q)
	}
}

// TestRollbackRefusesManualMode: pool_mode=manual must reject Rollback
// (use clear-emergency instead).
func TestRollbackRefusesManualMode(t *testing.T) {
	s := newTestScorer(emergencyCfg([]string{"a", "b"}, nil))
	s.renderer = &emergencyTestRenderer{path: t.TempDir() + "/cfg.yaml"}
	_, err := s.Rollback(1, 0)
	if err == nil {
		t.Error("expected error in manual mode")
	}
}

// TestRollbackQuarantineBlocksKGatingPromotion: a quarantined node must
// not be re-added to the pool by the next K-gating round.
func TestRollbackQuarantineBlocksKGatingPromotion(t *testing.T) {
	s := newTestScorer(emergencyCfg(nil, nil))
	s.cfg.PoolMode = "auto"
	now := time.Now()
	s.rollbackQuarantine = map[string]time.Time{
		"x": now.Add(1 * time.Hour),
	}

	if !s.inQuarantine("x", now) {
		t.Error("x should be in quarantine while inside the until window")
	}
	if s.inQuarantine("x", now.Add(2*time.Hour)) {
		t.Error("x should NOT be in quarantine past until")
	}
	if s.inQuarantine("y", now) {
		t.Error("y was never quarantined")
	}
}

// TestRollbackEmptyAuditLogIsNoop: when there are no transitions to
// undo, Rollback returns success with applied=0.
func TestRollbackEmptyAuditLogIsNoop(t *testing.T) {
	s := newTestScorer(emergencyCfg(nil, nil))
	s.cfg.PoolMode = "auto"
	s.renderer = &emergencyTestRenderer{path: t.TempDir() + "/cfg.yaml"}
	s.poolSet = map[string]bool{"a": true, "b": true}

	res, err := s.Rollback(5, 1*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.StepsApplied != 0 {
		t.Errorf("StepsApplied = %d, want 0", res.StepsApplied)
	}
}
