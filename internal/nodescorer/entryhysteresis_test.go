package nodescorer

import (
	"testing"
	"time"
)

// eligibilityScorer builds a scorer wired for sizing_mode=eligibility with
// the given ReadmitStrikes, plus nodeState entries for the named nodes.
func eligibilityScorer(readmit int, names ...string) *Scorer {
	cfg := defaultCfg()
	cfg.ReadmitStrikes = readmit
	cfg.SizingMode = "eligibility"
	cfg.PoolSizing.MinPoolSize = 1 // keep the redundancy floor out of these assertions
	s := newTestScorer(cfg)
	s.state = map[string]*nodeState{}
	for _, n := range names {
		s.state[n] = &nodeState{}
	}
	return s
}

// goodNode is a node that passes both the liveness gate and every admission
// quality threshold.
func goodNode(name string) NodeHealth {
	return NodeHealth{
		Name: name, Alive: true, Qualified: true,
		ProbeCount: 10, RTTP50Ms: 200, RTTP95Ms: 210, JitterMs: 10, FailRate: 0,
	}
}

// luckySampleNode is the real 92 failure shape: a node whose TRUE loss is
// ~40% but whose current 10-probe window happens to show 2 failures, so it
// clears max_fail_rate=0.25 this round. Indistinguishable, in a single
// round, from a genuinely good node — which is the entire problem.
func luckySampleNode(name string) NodeHealth {
	h := goodNode(name)
	h.FailRate = 0.20
	return h
}

// TestEntryHysteresis_LuckyRoundDoesNotAdmit is the regression test for the
// 92 flap (Hutao/美国_BGP_E, 6 pool updates in 40min). With
// ReadmitStrikes=2 a non-member that passes the quality gate for ONE round
// must not be admitted; it is admitted on the round its run reaches 2.
func TestEntryHysteresis_LuckyRoundDoesNotAdmit(t *testing.T) {
	const node = "Hutao/BGP_E"
	s := eligibilityScorer(2, node, "incumbent")
	// A non-empty poolSet: bootstrap exemption must not apply.
	s.poolSet["incumbent"] = true

	nodes := []NodeHealth{goodNode("incumbent"), luckySampleNode(node)}

	// Round 1: passes quality, run reaches 1 < 2 → held out.
	pool := map[string]bool{}
	s.eligibilityPoolLocked(nodes, pool, 1, time.Now())
	if pool[node] {
		t.Fatalf("admitted on a single lucky round: this is the 92 flap")
	}
	if got := s.state[node].admitRuns; got != 1 {
		t.Fatalf("admitRuns = %d, want 1", got)
	}

	// Round 2: still passing → run reaches 2 → admitted.
	pool = map[string]bool{}
	s.eligibilityPoolLocked(nodes, pool, 1, time.Now())
	if !pool[node] {
		t.Fatalf("not admitted after %d consecutive passing rounds", s.state[node].admitRuns)
	}
}

// TestEntryHysteresis_RunResetsOnRefusal verifies the run must be
// CONSECUTIVE. A node alternating pass/refuse (the observed 0.20/0.80
// oscillation) never accumulates a run and so never enters.
func TestEntryHysteresis_RunResetsOnRefusal(t *testing.T) {
	const node = "Hutao/BGP_A"
	s := eligibilityScorer(2, node, "incumbent")
	s.poolSet["incumbent"] = true

	good := []NodeHealth{goodNode("incumbent"), luckySampleNode(node)}
	bad := []NodeHealth{goodNode("incumbent"), func() NodeHealth {
		h := luckySampleNode(node)
		h.FailRate = 0.80 // refused: > max_fail_rate 0.25
		return h
	}()}

	for round := 0; round < 6; round++ {
		set := good
		if round%2 == 1 {
			set = bad
		}
		pool := map[string]bool{}
		s.eligibilityPoolLocked(set, pool, 1, time.Now())
		if pool[node] {
			t.Fatalf("round %d: flapping node admitted; admitRuns=%d",
				round, s.state[node].admitRuns)
		}
	}
}

// TestEntryHysteresis_IncumbentExempt is the guard that this change does not
// re-introduce the per-round churn eligibility mode exists to remove. An
// in-pool node is judged on liveness only and stays regardless of the
// quality gate or any accumulated run.
func TestEntryHysteresis_IncumbentExempt(t *testing.T) {
	const node = "incumbent"
	s := eligibilityScorer(3, node)
	s.poolSet[node] = true

	// Quality far outside every admission threshold, but Qualified (alive).
	h := goodNode(node)
	h.FailRate = 0.80
	h.RTTP95Ms = 5000

	pool := map[string]bool{}
	s.eligibilityPoolLocked([]NodeHealth{h}, pool, 1, time.Now())
	if !pool[node] {
		t.Fatal("incumbent dropped by the entry gate: this would be Plan-A churn")
	}
	if got := s.state[node].admitRuns; got != 0 {
		t.Fatalf("in-pool admitRuns = %d, want 0 (held at 0 so an exit re-earns entry)", got)
	}
}

// TestEntryHysteresis_ExitedNodeReEarnsEntry pins the reason admitRuns is
// held at 0 while in-pool: a node that exits must not be readmitted on the
// strength of a run it banked before it was ever admitted.
func TestEntryHysteresis_ExitedNodeReEarnsEntry(t *testing.T) {
	const node = "Hutao/BGP_B"
	s := eligibilityScorer(2, node, "other")
	s.poolSet["other"] = true

	nodes := []NodeHealth{goodNode("other"), luckySampleNode(node)}
	// Two passing rounds → admitted.
	for i := 0; i < 2; i++ {
		pool := map[string]bool{}
		s.eligibilityPoolLocked(nodes, pool, 1, time.Now())
		s.poolSet = pool
		s.poolSet["other"] = true
	}
	if !s.poolSet[node] {
		t.Fatal("precondition: node should be in pool after 2 passing rounds")
	}

	// It exits by failing the liveness gate (recentOk==0 → Qualified false).
	dead := luckySampleNode(node)
	dead.Alive, dead.Qualified = false, false
	pool := map[string]bool{}
	s.eligibilityPoolLocked([]NodeHealth{goodNode("other"), dead}, pool, 1, time.Now())
	s.poolSet = pool
	s.poolSet["other"] = true
	if s.poolSet[node] {
		t.Fatal("precondition: unqualified node should have exited")
	}

	// One lucky round is not enough to come straight back.
	pool = map[string]bool{}
	s.eligibilityPoolLocked(nodes, pool, 1, time.Now())
	if pool[node] {
		t.Fatal("readmitted after one round: exit must re-earn entry from scratch")
	}
}

// TestEntryHysteresis_BootstrapExempt verifies a cold start still fills the
// pool in one round. Without the exemption the data plane would have no
// us-pool for ReadmitStrikes rounds after every restart.
func TestEntryHysteresis_BootstrapExempt(t *testing.T) {
	s := eligibilityScorer(3, "a", "b")
	// poolSet empty = bootstrap.
	pool := map[string]bool{}
	s.eligibilityPoolLocked([]NodeHealth{goodNode("a"), goodNode("b")}, pool, 1, time.Now())
	if !pool["a"] || !pool["b"] {
		t.Fatalf("bootstrap did not fill in one round: pool=%v", pool)
	}
}

// TestEntryHysteresis_DisabledByDefault verifies ReadmitStrikes=1 (the
// default) preserves the previous single-round admission behavior, so this
// change is opt-in per deployment.
func TestEntryHysteresis_DisabledByDefault(t *testing.T) {
	const node = "fresh"
	s := eligibilityScorer(1, node, "incumbent")
	s.poolSet["incumbent"] = true

	pool := map[string]bool{}
	s.eligibilityPoolLocked([]NodeHealth{goodNode("incumbent"), goodNode(node)}, pool, 1, time.Now())
	if !pool[node] {
		t.Fatal("ReadmitStrikes=1 must admit on the first passing round")
	}
}
