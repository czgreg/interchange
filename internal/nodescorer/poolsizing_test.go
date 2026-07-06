package nodescorer

import (
	"testing"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

// TestComputeK exercises the K formula across the boundary cases that
// matter operationally: disabled (T=0), tiny T (K_min floor), normal T
// (K_demand wins), large T (K_max ceiling), supply-limited.
func TestComputeK(t *testing.T) {
	defaults := config.PoolSizingConfig{
		CapPerNode: 10,
		Surge:      1.5,
		KMin:       2,
		KMax:       12,
	}
	cases := []struct {
		name              string
		t                 int
		qualified         int
		wantK             int
		wantSupplyLimited bool
		overrides         func(*config.PoolSizingConfig)
	}{
		{
			name:      "t_active=0 uses default of 50",
			t:         0,
			qualified: 22,
			// TActive defaults to 50: kDemand=⌈50*1.5/10⌉=8, kFloor=max(3,⌈50/15⌉+1)=4 → K=8
			wantK: 8,
		},
		{
			name:      "tiny T pinned to KMin floor",
			t:         5,
			qualified: 22,
			// kDemand=⌈5*1.5/10⌉=1, kFloor=max(2, ⌈5/15⌉+1)=2 → K=2
			wantK: 2,
		},
		{
			name:      "T=20 dominated by failure-resilience floor",
			t:         20,
			qualified: 22,
			// kDemand=⌈20*1.5/10⌉=3, kFloor=⌈20/15⌉+1=3 → K=3
			wantK: 3,
		},
		{
			name:      "T=50 demand wins (the production target)",
			t:         50,
			qualified: 22,
			// kDemand=⌈50*1.5/10⌉=8, kFloor=⌈50/15⌉+1=5 → K=8
			wantK: 8,
		},
		{
			name:      "T=80 still under K_max",
			t:         80,
			qualified: 22,
			// kDemand=⌈80*1.5/10⌉=12, kFloor=⌈80/15⌉+1=7 → K=12
			wantK: 12,
		},
		{
			name:      "T=200 capped at K_max",
			t:         200,
			qualified: 22,
			// kDemand=30 → capped at K_max=12
			wantK: 12,
		},
		{
			name:              "supply-limited",
			t:                 50,
			qualified:         5,
			wantK:             5,
			wantSupplyLimited: true,
		},
		{
			name:      "custom KMax raises ceiling",
			t:         200,
			qualified: 22,
			wantK:     20,
			overrides: func(p *config.PoolSizingConfig) { p.KMax = 20 },
		},
		{
			name:      "default fill-in when fields are zero",
			t:         50,
			qualified: 22,
			overrides: func(p *config.PoolSizingConfig) {
				p.CapPerNode = 0
				p.Surge = 0
				p.KMin = 0
				p.KMax = 0
			},
			wantK: 8, // same as defaults
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := defaults
			p.TActive = tc.t
			if tc.overrides != nil {
				tc.overrides(&p)
			}
			k, sl := computeK(p, tc.qualified)
			if k != tc.wantK {
				t.Errorf("k: got %d want %d", k, tc.wantK)
			}
			if sl != tc.wantSupplyLimited {
				t.Errorf("supplyLimited: got %v want %v", sl, tc.wantSupplyLimited)
			}
		})
	}
}

// TestCompositeScore checks the ranking is monotonic in each input dimension
// and folds in passive fail rate when present.
func TestCompositeScore(t *testing.T) {
	base := NodeHealth{
		ProbeCount: 10,
		RTTP95Ms:   200,
		JitterMs:   20,
		FailRate:   0.0,
	}
	baseScore := compositeScore(base)

	// Higher p95 must score worse.
	worsep95 := base
	worsep95.RTTP95Ms = 400
	if compositeScore(worsep95) <= baseScore {
		t.Error("higher p95 should score higher (worse)")
	}

	// Higher jitter must score worse.
	worseJit := base
	worseJit.JitterMs = 200
	if compositeScore(worseJit) <= baseScore {
		t.Error("higher jitter should score higher (worse)")
	}

	// Higher probe fail_rate must score worse.
	worseFail := base
	worseFail.FailRate = 0.3
	if compositeScore(worseFail) <= baseScore {
		t.Error("higher probe fail_rate should score higher (worse)")
	}

	// Passive failures fold in only when there's a closed-window sample.
	withPassive := base
	withPassive.Passive = &PassiveStats{ClosedWindow: 10, FailRate: 0.5}
	if compositeScore(withPassive) <= baseScore {
		t.Error("passive failures should score higher (worse)")
	}
	emptyPassive := base
	emptyPassive.Passive = &PassiveStats{ClosedWindow: 0, FailRate: 0.5}
	if compositeScore(emptyPassive) != baseScore {
		t.Error("passive with zero closed-window must not move the score")
	}

	// No-probe-history sentinel: zero-history nodes must rank BELOW any
	// real-data node, including extremely poor ones. Otherwise they
	// would sweep the top-K with a free score=0 — observed on 89 right
	// after the K-gating deploy when 14 fishcloud/ctc-02 nodes had
	// probe_count=0 but were ranked first with all-zero metrics.
	noHistory := NodeHealth{ProbeCount: 0}
	terrible := NodeHealth{ProbeCount: 10, RTTP95Ms: 5000, JitterMs: 2000, FailRate: 0.9}
	if compositeScore(noHistory) <= compositeScore(terrible) {
		t.Errorf("no-history (%.0f) must score worse than terrible-but-measured (%.0f)",
			compositeScore(noHistory), compositeScore(terrible))
	}
}

// TestCompositeScoreQuadraticFail pins the relative ordering that
// matters in production: a "completely failed" node (fail=1.0) MUST
// rank worse than a "healthy but slow" node (p95=2858ms, fail=0). With
// a linear fail term, fail=1.0 contributed only 1000 to score and
// healthy-slow scored ~2870, ranking the dead node BETTER. The 92
// production incident traced to that inversion.
//
// The fix uses fail_rate² × 5000:
//   fail=1.0 → 5000  (catastrophic, well above any plausible latency)
//   fail=0.5 → 1250  (degraded but real)
//   fail=0.25 → 312  (small failures barely move score, latency dominates)
func TestCompositeScoreQuadraticFail(t *testing.T) {
	dead := NodeHealth{ProbeCount: 10, RTTP95Ms: 0, JitterMs: 0, FailRate: 1.0}
	healthySlow := NodeHealth{ProbeCount: 10, RTTP95Ms: 2858, JitterMs: 100, FailRate: 0.0}
	if compositeScore(dead) < compositeScore(healthySlow) {
		t.Errorf("dead (%.0f) must rank WORSE than healthy-slow (%.0f)",
			compositeScore(dead), compositeScore(healthySlow))
	}

	// Smaller failures should be roughly comparable to latency in score
	// terms (the quadratic still keeps them on the same order).
	smallFail := NodeHealth{ProbeCount: 10, RTTP95Ms: 200, JitterMs: 10, FailRate: 0.10}
	healthyFast := NodeHealth{ProbeCount: 10, RTTP95Ms: 200, JitterMs: 10, FailRate: 0.0}
	// fail=0.1 contributes 50 → tiny bump above zero-fail baseline.
	if compositeScore(smallFail)-compositeScore(healthyFast) > 100 {
		t.Errorf("small fail (10%%) shouldn't dominate score: %.0f vs %.0f",
			compositeScore(smallFail), compositeScore(healthyFast))
	}
}
// score()'s top-K cut: best-quality nodes win the pool slots.
func TestRankedSelectionPreservesQualityOrder(t *testing.T) {
	nodes := []NodeHealth{
		{Name: "slow", ProbeCount: 10, RTTP95Ms: 800, JitterMs: 200, FailRate: 0.2},
		{Name: "good", ProbeCount: 10, RTTP95Ms: 250, JitterMs: 10, FailRate: 0.0},
		{Name: "ok", ProbeCount: 10, RTTP95Ms: 350, JitterMs: 50, FailRate: 0.05},
		{Name: "dying", ProbeCount: 10, RTTP95Ms: 1500, JitterMs: 800, FailRate: 0.5},
	}
	// Same ranking the scorer would do.
	type pair struct {
		name  string
		score float64
	}
	scored := make([]pair, len(nodes))
	for i, n := range nodes {
		scored[i] = pair{n.Name, compositeScore(n)}
	}
	want := []string{"good", "ok", "slow", "dying"}
	// Insertion-sort by score for deterministic comparison.
	for i := 1; i < len(scored); i++ {
		for j := i; j > 0 && scored[j].score < scored[j-1].score; j-- {
			scored[j], scored[j-1] = scored[j-1], scored[j]
		}
	}
	for i, name := range want {
		if scored[i].name != name {
			t.Errorf("rank %d: got %q want %q (full=%+v)", i, scored[i].name, name, scored)
		}
	}
}
