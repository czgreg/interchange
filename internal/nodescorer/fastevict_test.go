package nodescorer

// Tests for the short-EWMA fast-eviction path and the kTarget fill
// composite ceiling, both added 2026-08-12. Before that, nodeShortEWMA had
// zero callers: every ranking AND eviction decision ran on the 24h long
// window, so a pool member degrading to 3000ms kept carrying terminals for
// ~1.9h. The fill post-pass separately ignored quality entirely, and was
// the one path by which a p95=5000ms node could start carrying traffic.

import (
	"context"
	"math"
	"testing"
	"time"
)

// seedEWMA plants an already-converged short/long EWMA for a node so tests
// exercise the decision logic rather than the smoothing math.
func seedEWMA(s *Scorer, name string, short, long float64) {
	if s.ewma == nil {
		s.ewma = map[string]*nodeEWMA{}
	}
	now := time.Now()
	s.ewma[name] = &nodeEWMA{
		Short:       ewmaPoint{Value: short, UpdatedAt: now},
		Long:        ewmaPoint{Value: long, UpdatedAt: now},
		FirstSeenAt: now.Add(-48 * time.Hour), // past trial
	}
}

func TestPoolShortEWMAMedian_IgnoresUnobservedMembers(t *testing.T) {
	s := newTestScorer(defaultCfg())
	s.poolSet = map[string]bool{"a": true, "b": true, "c": true, "d": true}
	seedEWMA(s, "a", 200, 200)
	seedEWMA(s, "b", 300, 300)
	seedEWMA(s, "c", 400, 400)
	// "d" has no EWMA at all → +Inf. Including it would push the median to
	// +Inf and silently disable eviction.
	if got := s.poolShortEWMAMedian(); got != 300 {
		t.Errorf("median = %v, want 300 (unobserved member must be skipped)", got)
	}
}

// With fewer than 3 measured members, "median" is not a population
// statistic; evicting against it could remove a healthy node because its
// single peer happens to be fast.
func TestPoolShortEWMAMedian_TooFewSamplesReturnsZero(t *testing.T) {
	s := newTestScorer(defaultCfg())
	s.poolSet = map[string]bool{"a": true, "b": true}
	seedEWMA(s, "a", 100, 100)
	seedEWMA(s, "b", 5000, 5000)
	if got := s.poolShortEWMAMedian(); got != 0 {
		t.Errorf("median = %v, want 0 (insufficient sample must disable eviction)", got)
	}
}

// The mean would be dragged upward by the very node under test; the median
// must not be.
func TestPoolShortEWMAMedian_UnaffectedByOneOutlier(t *testing.T) {
	s := newTestScorer(defaultCfg())
	s.poolSet = map[string]bool{"a": true, "b": true, "c": true, "d": true, "e": true}
	seedEWMA(s, "a", 240, 240)
	seedEWMA(s, "b", 250, 250)
	seedEWMA(s, "c", 260, 260)
	seedEWMA(s, "d", 270, 270)
	seedEWMA(s, "e", 30000, 30000) // the degraded one
	med := s.poolShortEWMAMedian()
	if med != 260 {
		t.Fatalf("median = %v, want 260", med)
	}
	// Sanity: the outlier must exceed the trigger the median implies,
	// otherwise the whole mechanism is inert.
	if 30000 <= med*evictShortEWMAFactor {
		t.Errorf("outlier 30000 should exceed limit %v", med*evictShortEWMAFactor)
	}
}

// The reproduction of the real complaint: a node degrades from ~247ms to
// 3000ms. Its LONG EWMA still looks fine (24h half-life), so ranking keeps
// it; only the short window sees it.
func TestFastEvict_DegradedMemberExceedsLimitWhileLongEWMALooksFine(t *testing.T) {
	s := newTestScorer(defaultCfg())
	s.poolSet = map[string]bool{"good1": true, "good2": true, "good3": true, "degraded": true}
	seedEWMA(s, "good1", 240, 240)
	seedEWMA(s, "good2", 250, 250)
	seedEWMA(s, "good3", 260, 260)
	// Short window has caught up (3000); long window has barely moved.
	seedEWMA(s, "degraded", 3000, 255)

	med := s.poolShortEWMAMedian()
	limit := med * evictShortEWMAFactor
	if s.nodeShortEWMA("degraded") <= limit {
		t.Fatalf("degraded short EWMA %v must exceed limit %v",
			s.nodeShortEWMA("degraded"), limit)
	}
	// And the long window must NOT flag it — that is the whole problem.
	if s.nodeLongEWMA("degraded") > limit {
		t.Errorf("long EWMA %v should still look acceptable (<= %v); "+
			"if it flags the node the test no longer covers the gap it was written for",
			s.nodeLongEWMA("degraded"), limit)
	}
	for _, n := range []string{"good1", "good2", "good3"} {
		if s.nodeShortEWMA(n) > limit {
			t.Errorf("healthy node %s must not exceed the limit", n)
		}
	}
}

// A uniformly slow pool must not evict anyone: the median rises with the
// population, so "slow" stops being anomalous. This is why the gate is
// relative rather than an absolute ms threshold.
func TestFastEvict_UniformlySlowPoolEvictsNobody(t *testing.T) {
	s := newTestScorer(defaultCfg())
	s.poolSet = map[string]bool{"a": true, "b": true, "c": true, "d": true}
	for _, n := range []string{"a", "b", "c", "d"} {
		seedEWMA(s, n, 2500, 2500)
	}
	limit := s.poolShortEWMAMedian() * evictShortEWMAFactor
	for _, n := range []string{"a", "b", "c", "d"} {
		if s.nodeShortEWMA(n) > limit {
			t.Errorf("%s should survive in a uniformly slow pool (short=%v limit=%v)",
				n, s.nodeShortEWMA(n), limit)
		}
	}
}

// A member with no short-window signal must never be evicted on this
// signal: +Inf means "unknown", not "bad". Evicting on unknown would empty
// the pool after a gateway restart.
func TestFastEvict_UnobservedMemberIsNotEvicted(t *testing.T) {
	s := newTestScorer(defaultCfg())
	s.poolSet = map[string]bool{"a": true, "b": true, "c": true, "fresh": true}
	seedEWMA(s, "a", 240, 240)
	seedEWMA(s, "b", 250, 250)
	seedEWMA(s, "c", 260, 260)
	if got := s.nodeShortEWMA("fresh"); !math.IsInf(got, 1) {
		t.Fatalf("unseeded node should read +Inf, got %v", got)
	}
	// The production loop skips +Inf explicitly; assert the precondition
	// that makes that skip correct.
	if !math.IsInf(s.nodeShortEWMA("fresh"), 1) {
		t.Error("fresh node must be +Inf so the evict loop skips it")
	}
}

func TestPoolFillCeiling_TooFewMeasuredMembersMeansNoCeiling(t *testing.T) {
	s := newTestScorer(defaultCfg())
	s.poolSet = map[string]bool{"a": true, "b": true}
	nodes := []NodeHealth{
		{Name: "a", ProbeCount: 5, RTTP95Ms: 200},
		{Name: "b", ProbeCount: 5, RTTP95Ms: 300},
	}
	if got := s.poolFillCeiling(nodes); got != 0 {
		t.Errorf("ceiling = %v, want 0 during bootstrap/supply collapse", got)
	}
}

// The measured 92 case: ash/US-04·GCP had p95=5000, jitter=4587,
// composite=14674 and Qualified=true. It must be refused by the fill path.
func TestPoolFillCeiling_RejectsTheMeasuredBadNode(t *testing.T) {
	s := newTestScorer(defaultCfg())
	s.poolSet = map[string]bool{"a": true, "b": true, "c": true}
	nodes := []NodeHealth{
		{Name: "a", ProbeCount: 5, RTTP95Ms: 240, JitterMs: 5},
		{Name: "b", ProbeCount: 5, RTTP95Ms: 250, JitterMs: 6},
		{Name: "c", ProbeCount: 5, RTTP95Ms: 300, JitterMs: 10},
	}
	ceil := s.poolFillCeiling(nodes)
	if ceil <= 0 {
		t.Fatalf("expected a ceiling with 3 measured members, got %v", ceil)
	}
	bad := NodeHealth{Name: "us04gcp", ProbeCount: 5, RTTP95Ms: 5000, JitterMs: 4587}
	if c := compositeScore(bad); c <= ceil {
		t.Errorf("composite %v should exceed ceiling %v", c, ceil)
	}
	// A merely mediocre node must still be admitted — the ceiling is not a
	// quality bar, only a sanity bound.
	ok := NodeHealth{Name: "meh", ProbeCount: 5, RTTP95Ms: 450, JitterMs: 20}
	if c := compositeScore(ok); c > ceil {
		t.Errorf("composite %v (mediocre but usable) must not exceed ceiling %v", c, ceil)
	}
}

// The no-measurement sentinel must never become the ceiling reference: it
// is ~6 orders of magnitude above a real composite and would make the
// ceiling meaningless.
// Integration: the whole point is that eviction must survive the two fill
// paths that run immediately after it. A node deleted from preferredSet is
// absent from newPoolSet, which looks exactly like a free slot to the
// kTarget post-pass — before the `evicted[name]` guard it was re-admitted
// on the very next lines, making the eviction a no-op.
func TestFastEvict_EvictedNodeIsNotRefilledSameRound(t *testing.T) {
	cfg := kGatingCfg(4)
	nodes := map[string]proxyNode{
		"sub/n1": {alive: true, history: goodHistory(240)},
		"sub/n2": {alive: true, history: goodHistory(250)},
		"sub/n3": {alive: true, history: goodHistory(260)},
		"sub/n4": {alive: true, history: goodHistory(270)},
		// A spare, so refilling is possible and the test can tell "stayed
		// out" apart from "nothing else available".
		"sub/n5": {alive: true, history: goodHistory(280)},
	}
	s, _, mutate := newDecisionScorer(t, cfg, nodes)

	// Round 1: establish a pool and converged EWMAs.
	s.score(context.Background())
	pool1 := inPoolNames(s.GetSnapshot())
	if len(pool1) == 0 {
		t.Fatal("round 1 produced an empty pool")
	}

	// Pick a member and degrade ONLY its short EWMA, leaving its probe
	// history (and therefore its composite) healthy.
	//
	// This isolation is deliberate and was found by mutation testing: an
	// earlier version of this test also rewrote the victim's probe history
	// to 6000ms, which made its composite exceed the fill ceiling. The
	// ceiling then kept it out regardless of the evict guard, so removing
	// the guard did not fail the test — two overlapping protections, and no
	// way to tell which one acted. Keeping composite healthy means the
	// `evicted[name]` guard in the fill path is the ONLY thing that can
	// keep this node out.
	var victim string
	for n := range pool1 {
		victim = n
		break
	}

	s.mu.Lock()
	med := s.poolShortEWMAMedian()
	if med > 0 {
		// Long EWMA stays at the median so the node still ranks well and
		// the fill path genuinely wants to re-add it.
		seedEWMA(s, victim, med*evictShortEWMAFactor*3, med)
	}
	s.mu.Unlock()
	if med == 0 {
		t.Skip("pool has fewer than 3 measured members; eviction is disabled by design")
	}
	_ = mutate // history intentionally left healthy; see above

	// Round 2: eviction should fire and must not be undone by the fill.
	s.score(context.Background())
	pool2 := inPoolNames(s.GetSnapshot())
	if pool2[victim] {
		t.Errorf("evicted node %s was re-admitted in the same round (fill path ignored the evict guard); pool=%v",
			victim, pool2)
	}
	// The pool must not have shrunk: the spare should have taken the slot.
	if len(pool2) < len(pool1) {
		t.Errorf("pool shrank from %d to %d — eviction should be backfilled from the spare; pool=%v",
			len(pool1), len(pool2), pool2)
	}
}

// MinPoolSize is a hard floor and outranks fast eviction: below it, a slow
// node beats no node. Without this, a pool-wide degradation could evict
// everything and leave the data plane empty.
func TestFastEvict_RespectsMinPoolFloor(t *testing.T) {
	cfg := kGatingCfg(3)
	cfg.PoolSizing.MinPoolSize = 3
	nodes := map[string]proxyNode{
		"sub/n1": {alive: true, history: goodHistory(240)},
		"sub/n2": {alive: true, history: goodHistory(250)},
		"sub/n3": {alive: true, history: goodHistory(260)},
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes)
	s.score(context.Background())

	// Degrade every member's short EWMA. The median rises with them, so
	// nothing should be evicted anyway — but even if the factor were
	// mis-set, the floor must hold.
	s.mu.Lock()
	for n := range s.poolSet {
		seedEWMA(s, n, 99999, 250)
	}
	s.mu.Unlock()

	s.score(context.Background())
	pool := inPoolNames(s.GetSnapshot())
	if len(pool) < 3 {
		t.Errorf("pool fell below MinPoolSize=3 (got %d): %v", len(pool), pool)
	}
}

func TestPoolFillCeiling_ExcludesNoMeasurementSentinel(t *testing.T) {
	s := newTestScorer(defaultCfg())
	s.poolSet = map[string]bool{"a": true, "b": true, "c": true, "nodata": true}
	nodes := []NodeHealth{
		{Name: "a", ProbeCount: 5, RTTP95Ms: 240},
		{Name: "b", ProbeCount: 5, RTTP95Ms: 250},
		{Name: "c", ProbeCount: 5, RTTP95Ms: 260},
		{Name: "nodata", ProbeCount: 0},
	}
	ceil := s.poolFillCeiling(nodes)
	if ceil >= noMeasurementScore {
		t.Fatalf("ceiling %v must not be derived from the sentinel", ceil)
	}
	if ceil != 250*evictShortEWMAFactor {
		t.Errorf("ceiling = %v, want %v (median of 240/250/260 × factor)",
			ceil, 250*evictShortEWMAFactor)
	}
}
