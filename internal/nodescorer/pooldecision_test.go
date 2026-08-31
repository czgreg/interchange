// pooldecision_test.go — characterization tests for the score() decision
// path: the K-gating pool-selection logic that picks which nodes carry
// production traffic. Before this file that ~350-line branch (scorer.go
// 405-940) had ZERO test coverage — every regression in it (Plan A's 4
// pitfalls, the quadratic-fail inversion, the Qualified gate) was caught
// only by the two live nodes. These tests drive the real score() through
// a mock clash-api so the decisions are pinned in CI.
//
// They are CHARACTERIZATION tests: they assert what the code does today,
// not necessarily what it should do. Where current behavior is a known
// gap (see TestDecision_DegradedNotEvictedFast) the test says so in a
// comment, so when the behavior is fixed the test flips loudly rather
// than silently passing.

package nodescorer

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
	"github.com/leap-gateway/leap-gateway/internal/subscribe"
)

// fakeRenderer records the (usPool, routingMembers) it was last asked to
// render so a test can assert what the hot-reload would have shipped.
// Path points at a temp file so writeAtomic + saveStateLocked succeed.
type fakeRenderer struct {
	path        string
	mu          sync.Mutex
	lastUsPool  []string
	lastRouting []string
	renders     int
}

func (f *fakeRenderer) RenderWithPools(_ []subscribe.Outbound, usPool, routing []string, _ map[string][]string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastUsPool = append([]string(nil), usPool...)
	f.lastRouting = append([]string(nil), routing...)
	f.renders++
	return []byte("rendered: " + strings.Join(routing, ",")), nil
}

func (f *fakeRenderer) Path() string { return f.path }

func (f *fakeRenderer) snapshot() (usPool, routing []string, renders int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lastUsPool...), append([]string(nil), f.lastRouting...), f.renders
}

// waitRenders blocks until the renderer has been called at least n times
// or the deadline passes. score() fires hotReload on a goroutine, so a
// test that asserts on render output must synchronize on it.
func (f *fakeRenderer) waitRenders(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		got := f.renders
		f.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("renderer not called %d time(s) within deadline (got %d)", n, f.renders)
}

// proxyNode describes one node in the mock /proxies response: its probe
// delay history (0 = a failed probe) and alive bit.
type proxyNode struct {
	alive   bool
	history []int
}

// mockClashAPI stands in for mihomo's clash-api. It serves /proxies (with
// a us-pool load-balance group whose `all` lists every node) and
// /connections (empty — passive throughput isn't under test here). PUT
// /configs and /proxies/* (hot-reload + selector flips) return 204.
//
// Returns the api address plus a mutator that rewrites one node's probe
// history between scoring rounds, so a test can simulate a node degrading
// or recovering and assert the resulting pool transition.
func mockClashAPI(t *testing.T, probeURL string, nodes map[string]proxyNode) (addr string, mutate func(tag string, history []int)) {
	t.Helper()
	var mu sync.Mutex
	proxies := map[string]map[string]interface{}{}
	all := make([]interface{}, 0, len(nodes))
	for tag, n := range nodes {
		all = append(all, tag)
		proxies[tag] = buildProxyEntry(n.alive, probeURL, n.history)
	}
	proxies["us-pool"] = map[string]interface{}{
		"type": "LoadBalance",
		"all":  all,
		"now":  "",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/proxies":
			writeTestJSON(w, map[string]interface{}{"proxies": proxies})
		case r.Method == http.MethodGet && r.URL.Path == "/connections":
			writeTestJSON(w, map[string]interface{}{"connections": []interface{}{}})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	mutate = func(tag string, history []int) {
		mu.Lock()
		defer mu.Unlock()
		proxies[tag] = buildProxyEntry(true, probeURL, history)
	}
	// strip "http://" — apiAddr is host:port (score() prepends the scheme).
	return strings.TrimPrefix(srv.URL, "http://"), mutate
}

func writeTestJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// inPoolNames extracts the InPool=true node names from a snapshot, for
// concise assertions.
func inPoolNames(snap Snapshot) map[string]bool {
	out := map[string]bool{}
	for _, n := range snap.Nodes {
		if n.InPool {
			out[n.Name] = true
		}
	}
	return out
}

// newDecisionScorer wires a Scorer to a mock clash-api + fake renderer for
// K-gating decision tests. probeURL matches what buildProxyEntry embeds so
// extractHistory finds the delay history. Returns a mutate func to rewrite
// a node's probe history between rounds.
func newDecisionScorer(t *testing.T, cfg config.NodeQualifyConfig, nodes map[string]proxyNode) (s *Scorer, fr *fakeRenderer, mutate func(tag string, history []int)) {
	t.Helper()
	const probeURL = "http://probe.test/generate_204"
	apiAddr, mut := mockClashAPI(t, probeURL, nodes)
	fr = &fakeRenderer{path: t.TempDir() + "/config.yaml"}
	s = New(cfg, nil, apiAddr, "", probeURL, "", fr, func() []subscribe.Outbound { return nil })
	// score() fires hotReload on a goroutine that writes config.yaml(.tmp)
	// into the TempDir. If the test returns before that goroutine finishes,
	// t.TempDir's RemoveAll races the write. Give in-flight renders a beat
	// to settle before cleanup runs.
	t.Cleanup(func() { time.Sleep(50 * time.Millisecond) })
	return s, fr, mut
}

// goodHistory is a clean 10-probe history (all ~150ms, no failures) — a
// node that should always qualify.
func goodHistory(rttMs int) []int {
	h := make([]int, 10)
	for i := range h {
		h[i] = rttMs
	}
	return h
}

// kGatingCfg returns an auto-mode K-gating config with the given target
// pool size. cap_per_node + t_active are chosen so computeK lands on
// wantK (t_active = wantK * cap / surge, surge=1.5).
func kGatingCfg(wantK int) config.NodeQualifyConfig {
	cfg := defaultCfg()
	cfg.ScoringInterval = 5 * time.Minute
	cfg.PoolMode = "auto"
	cfg.PoolSizing = config.PoolSizingConfig{
		TActive:    wantK * 10, // cap=10, surge=1.5 → kDemand = ceil(T*1.5/10)
		CapPerNode: 10,
		Surge:      1.0, // kDemand = ceil(T/cap) = wantK exactly
		KMin:       1,
		KMax:       12,
	}
	return cfg
}

// --- Bootstrap: first scoring round fills the pool to K from the best
// candidates, even though strikes/okRuns counters start at zero. This is
// the cold-start path; without the bootstrap branch the pool would stay
// empty for ReadmitStrikes rounds and starve the data plane.
func TestDecision_BootstrapFillsToK(t *testing.T) {
	cfg := kGatingCfg(5)
	nodes := map[string]proxyNode{
		"sub/n1": {alive: true, history: goodHistory(100)},
		"sub/n2": {alive: true, history: goodHistory(120)},
		"sub/n3": {alive: true, history: goodHistory(140)},
		"sub/n4": {alive: true, history: goodHistory(160)},
		"sub/n5": {alive: true, history: goodHistory(180)},
		"sub/n6": {alive: true, history: goodHistory(200)},
		"sub/n7": {alive: true, history: goodHistory(220)},
	}
	s, fr, _ := newDecisionScorer(t, cfg, nodes)
	s.score(context.Background())

	snap := s.GetSnapshot()
	k := snap.PoolSizing.KTarget
	if k < 4 || k > 7 {
		t.Fatalf("unexpected KTarget=%d (helper math drifted)", k)
	}
	pool := inPoolNames(snap)
	if len(pool) != k {
		t.Fatalf("bootstrap pool size = %d, want K=%d; pool=%v", len(pool), k, pool)
	}
	// Bootstrap must pick the K lowest-latency nodes (best compositeScore).
	// n1..n5 are the 5 fastest; with K=5 they're exactly the pool.
	if k == 5 {
		for _, want := range []string{"sub/n1", "sub/n2", "sub/n3", "sub/n4", "sub/n5"} {
			if !pool[want] {
				t.Errorf("bootstrap pool missing fastest node %s; pool=%v", want, pool)
			}
		}
	}
	// A pool change on a fresh start must hot-reload exactly once, and the
	// routing set it ships must equal the in-pool set.
	fr.waitRenders(t, 1)
	_, routing, renders := fr.snapshot()
	if renders != 1 {
		t.Errorf("expected 1 hot-reload on bootstrap, got %d", renders)
	}
	if len(routing) != len(pool) {
		t.Errorf("routing members (%d) != pool size (%d)", len(routing), len(pool))
	}
}

// --- us-pool (probing set) always contains ALL qualified candidates, not
// just the routing top-K. This is the property that answers the "out-of-
// pool nodes show data" question: every qualified node stays in us-pool,
// so mihomo keeps url-test probing it → its RTT history (probe_count,
// p50/p95, fail_rate) keeps refreshing even while it's NOT carrying
// production traffic. If this regresses, out-of-pool nodes' probe history
// freezes and the ranking locks in place (the bug the code comment at
// scorer.go:911-917 warns about).
func TestDecision_UsPoolKeepsAllQualifiedForProbing(t *testing.T) {
	cfg := kGatingCfg(5)
	nodes := map[string]proxyNode{}
	for _, n := range []struct {
		tag string
		rtt int
	}{
		{"sub/n1", 100}, {"sub/n2", 120}, {"sub/n3", 140}, {"sub/n4", 160},
		{"sub/n5", 180}, {"sub/n6", 200}, {"sub/n7", 220}, {"sub/n8", 240},
	} {
		nodes[n.tag] = proxyNode{alive: true, history: goodHistory(n.rtt)}
	}
	s, fr, _ := newDecisionScorer(t, cfg, nodes)
	s.score(context.Background())
	fr.waitRenders(t, 1)

	usPool, routing, _ := fr.snapshot()
	// All 8 qualified nodes must be in us-pool (probing set).
	if len(usPool) != 8 {
		t.Errorf("us-pool (probing set) = %d nodes, want all 8 qualified", len(usPool))
	}
	// Only K carry traffic.
	if len(routing) >= len(usPool) {
		t.Errorf("routing (%d) should be a strict subset of us-pool (%d)", len(routing), len(usPool))
	}
}

// --- Catastrophic gate: a node with fail_rate >= 0.9 across its full
// window is forced Qualified=false in scoreNode and therefore never enters
// the qualified ranking, no matter how good its surviving probes' latency
// is. Regression guard for 1bf72cd (the cyberguard fail=1.0 in-pool bug).
func TestDecision_CatastrophicFailExcludedFromPool(t *testing.T) {
	cfg := kGatingCfg(5)
	// 9 of 10 probes failed (delay=0) → fail_rate 0.9. The one surviving
	// probe is blazing fast (10ms) so latency-only ranking would love it.
	catastrophic := []int{10, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	nodes := map[string]proxyNode{
		"sub/dead":  {alive: true, history: catastrophic},
		"sub/good1": {alive: true, history: goodHistory(300)},
		"sub/good2": {alive: true, history: goodHistory(310)},
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes)
	s.score(context.Background())

	snap := s.GetSnapshot()
	for _, n := range snap.Nodes {
		if n.Name == "sub/dead" {
			if n.Qualified {
				t.Errorf("catastrophic node qualified=true, want false (reason=%q)", n.Reason)
			}
			if n.InPool {
				t.Error("catastrophic node must never be InPool despite 10ms surviving probe")
			}
		}
	}
}

// --- Eviction of a catastrophic-fail member when a healthy spare exists.
//
// CHARACTERIZATION FINDING: the EvictStrikes hysteresis (scorer.go:703,
// "stay in pool until strikes reach evict bar") is OVERRIDDEN by the
// exact-K post-pass (scorer.go:743-776) whenever a healthy spare is ready
// to backfill. When n4 goes catastrophic it drops from preferredSet; the
// top-K qualified set then promotes the spare, pushing |newPoolSet| over
// K; the over-capacity pass drops non-preferred members worst-first — and
// n4, being unqualified, is exactly such a member. Net effect: a
// catastrophic member with a healthy replacement is evicted on the NEXT
// round (strikes=1), not after EvictStrikes rounds.
//
// This is arguably the right behavior (don't keep a dead node when a good
// one is ready), but it means EvictStrikes only actually delays eviction
// in the narrow case where evicting would drop the pool below K (no spare
// available). If someone later relies on EvictStrikes giving a uniform
// 2-round grace period, this test flips and points them here.
func TestDecision_CatastrophicMemberEvictedWhenSpareReady(t *testing.T) {
	cfg := kGatingCfg(4)
	cfg.EvictStrikes = 2
	cfg.HotReloadMinInterval = 0
	nodes := map[string]proxyNode{
		"sub/n1": {alive: true, history: goodHistory(100)},
		"sub/n2": {alive: true, history: goodHistory(120)},
		"sub/n3": {alive: true, history: goodHistory(140)},
		"sub/n4": {alive: true, history: goodHistory(160)},
		"sub/n5": {alive: true, history: goodHistory(180)}, // healthy spare
	}
	s, _, mutate := newDecisionScorer(t, cfg, nodes)
	s.score(context.Background())
	got := inPoolNames(s.GetSnapshot())
	if !got["sub/n4"] || len(got) != 4 {
		t.Fatalf("round1: expected n4 in a 4-node pool; got %v", got)
	}

	// n4 goes catastrophic (fail_rate 0.9). With healthy spare n5 ready,
	// n4 is evicted next round despite EvictStrikes=2.
	mutate("sub/n4", []int{160, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	s.score(context.Background())
	got = inPoolNames(s.GetSnapshot())
	if got["sub/n4"] {
		t.Errorf("n4 should be evicted immediately when spare exists; pool=%v", got)
	}
	if !got["sub/n5"] {
		t.Errorf("healthy spare n5 should have backfilled; pool=%v", got)
	}
	if len(got) != 4 {
		t.Errorf("pool should stay at K=4; got %d (%v)", len(got), got)
	}
}

// --- Eviction hysteresis genuinely holds when there is NO spare: a pool
// member that drops out of the preferred top-K stays in the pool until
// EvictStrikes consecutive rounds, because evicting it early would drop
// the pool below K and the under-capacity pass would just re-add it. This
// is the case where EvictStrikes actually does something.
//
// We construct it with exactly K qualified nodes: when one ranks worst,
// there's no qualified replacement, so the pool can't shrink and the
// member rides out its strikes. (All nodes stay Qualified=true — only the
// ranking shifts — so kTarget stays at K.)
func TestDecision_HysteresisHoldsWithoutSpare(t *testing.T) {
	cfg := kGatingCfg(4)
	cfg.EvictStrikes = 2
	cfg.HotReloadMinInterval = 0
	// Exactly 4 qualified nodes, K=4 → no spare. The pool is all of them;
	// no ranking shuffle can drop the pool below K, so every member rides
	// out any transient strike. Assert the pool stays whole across rounds
	// even when one member's latency degrades (but stays qualified).
	nodes := map[string]proxyNode{
		"sub/n1": {alive: true, history: goodHistory(100)},
		"sub/n2": {alive: true, history: goodHistory(120)},
		"sub/n3": {alive: true, history: goodHistory(140)},
		"sub/n4": {alive: true, history: goodHistory(160)},
	}
	s, _, mutate := newDecisionScorer(t, cfg, nodes)
	s.score(context.Background())
	if len(inPoolNames(s.GetSnapshot())) != 4 {
		t.Fatalf("round1: expected all 4 nodes in pool")
	}
	// n4 latency degrades sharply but stays qualified (recent successes).
	mutate("sub/n4", goodHistory(2000))
	s.score(context.Background())
	got := inPoolNames(s.GetSnapshot())
	if !got["sub/n4"] || len(got) != 4 {
		t.Errorf("no-spare: degraded-but-qualified n4 must stay (pool can't shrink below K); pool=%v", got)
	}
}

// --- Out-of-pool nodes still carry full scoring data. This is the direct
// characterization of the dashboard "out-of-pool nodes show 0 data"
// question: a node that is NOT in the routing pool (InPool=false) but IS a
// qualified candidate still gets its RTT statistics computed every round
// (ProbeCount, p50/p95, FailRate). The ONLY field that is legitimately 0
// for an out-of-pool node is ThroughputBps — because that is measured from
// live /connections traffic, and an out-of-pool node carries no production
// traffic by definition. So a dashboard showing p50/p95/fail for out-of-
// pool nodes but throughput=0 is CORRECT, not a bug.
func TestDecision_OutOfPoolNodesRetainScoringData(t *testing.T) {
	cfg := kGatingCfg(4)
	cfg.HotReloadMinInterval = 0
	nodes := map[string]proxyNode{
		"sub/n1": {alive: true, history: goodHistory(100)},
		"sub/n2": {alive: true, history: goodHistory(120)},
		"sub/n3": {alive: true, history: goodHistory(140)},
		"sub/n4": {alive: true, history: goodHistory(160)},
		"sub/n5": {alive: true, history: goodHistory(180)}, // out of pool (K=4)
		"sub/n6": {alive: true, history: goodHistory(200)}, // out of pool
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes)
	s.score(context.Background())

	var outOfPool []NodeHealth
	for _, n := range s.GetSnapshot().Nodes {
		if !n.InPool {
			outOfPool = append(outOfPool, n)
		}
	}
	if len(outOfPool) == 0 {
		t.Fatal("expected some out-of-pool nodes with K=4 and 6 candidates")
	}
	for _, n := range outOfPool {
		// These fields MUST be populated even though the node isn't routing.
		if n.ProbeCount == 0 {
			t.Errorf("%s: out-of-pool node has probe_count=0 — its url-test history froze (regression of us-pool probing-set property)", n.Name)
		}
		if n.RTTP50Ms == 0 || n.RTTP95Ms == 0 {
			t.Errorf("%s: out-of-pool node missing RTT stats (p50=%d p95=%d) — should still be scored", n.Name, n.RTTP50Ms, n.RTTP95Ms)
		}
		// Throughput legitimately 0 for an out-of-pool node (no traffic).
		if n.ThroughputBps != 0 {
			t.Logf("%s: out-of-pool node has throughput=%.0f (unusual but not wrong)", n.Name, n.ThroughputBps)
		}
	}
}

// --- Benefit-of-doubt for brand-new nodes: a node with NO probe history
// yet (alive, but mihomo hasn't probed it) is marked Qualified=true so it
// can enter us-pool, get probed, and accumulate real data. Without this
// the node would be stuck Qualified=false forever (mihomo only probes
// group members) — the vicious cycle scoreNode:1021-1028 guards against.
// It still gets the sentinel compositeScore so it ranks BELOW measured
// nodes and doesn't sweep the top of a routing pool by luck.
func TestDecision_NewNodeBenefitOfDoubtButRanksLast(t *testing.T) {
	cfg := kGatingCfg(4)
	cfg.HotReloadMinInterval = 0
	nodes := map[string]proxyNode{
		"sub/fresh": {alive: true, history: nil}, // no history yet
		"sub/m1":    {alive: true, history: goodHistory(100)},
		"sub/m2":    {alive: true, history: goodHistory(120)},
		"sub/m3":    {alive: true, history: goodHistory(140)},
		"sub/m4":    {alive: true, history: goodHistory(160)},
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes)
	s.score(context.Background())

	var fresh *NodeHealth
	for i, n := range s.GetSnapshot().Nodes {
		if n.Name == "sub/fresh" {
			fresh = &s.GetSnapshot().Nodes[i]
			break
		}
	}
	if fresh == nil {
		t.Fatal("fresh node missing from snapshot")
	}
	if !fresh.Qualified {
		t.Errorf("new node should be qualified by benefit-of-doubt; reason=%q", fresh.Reason)
	}
	// With 4 measured nodes and K=4, the zero-history node ranks last and
	// is kept out of the routing pool — measured nodes win the slots.
	if fresh.InPool {
		t.Error("zero-history node should rank below measured nodes and stay out of a full pool")
	}
}

// --- Regression (2026-06-12): the no-measurement compositeScore sentinel
// (1e9) must NOT leak into the EWMA. Before the fix, score() fed the
// sentinel for any ProbeCount==0 node into updateNodeEWMA; the smoothed
// long EWMA then seeded at 1e9 and decayed at the 24h half-life, swamping
// real composites (~200–2800) for days. Every node's long EWMA converged
// to "the decayed sentinel", so the promote ranking + SwapThresholdScore
// gate compared differences of millions against a margin of 100 — noise —
// and the 8th pool slot flapped between two nodes ~18×/24h on 92.
//
// Here we drive one round with a measured node and one zero-history node,
// then assert the zero-history node carries NO EWMA signal (nodeLongEWMA
// stays +Inf) while the measured node's long EWMA equals its real
// composite — i.e. the sentinel never entered the smoothing.
func TestDecision_NoMeasurementSentinelDoesNotPoisonEWMA(t *testing.T) {
	cfg := kGatingCfg(4)
	cfg.HotReloadMinInterval = 0
	nodes := map[string]proxyNode{
		"sub/measured": {alive: true, history: goodHistory(150)},
		"sub/nodata":   {alive: true, history: nil}, // ProbeCount==0
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes)
	s.score(context.Background())

	s.mu.Lock()
	defer s.mu.Unlock()

	// Zero-history node: no EWMA signal at all (sentinel was skipped).
	if e, ok := s.ewma["sub/nodata"]; ok && !e.Long.UpdatedAt.IsZero() {
		t.Errorf("no-data node poisoned the EWMA: long value=%g (want: no signal)", e.Long.Value)
	}
	if got := s.nodeLongEWMA("sub/nodata"); !math.IsInf(got, 1) {
		t.Errorf("no-data node long EWMA = %g, want +Inf (no signal)", got)
	}

	// Measured node: long EWMA seeded at its real composite (~150ms p95,
	// no jitter/fail), nowhere near the 1e9 sentinel.
	got := s.nodeLongEWMA("sub/measured")
	if got >= poisonFloor {
		t.Errorf("measured node long EWMA = %g — sentinel leaked into smoothing", got)
	}
	if got <= 0 || got > 1000 {
		t.Errorf("measured node long EWMA = %g, want a real composite in the low hundreds", got)
	}
}

// --- Regression (2026-06-12): a state file already poisoned by the above
// bug must heal on load, not wait out the ~10-day decay. copyEWMA scrubs
// any point at/above poisonFloor back to "no signal".
func TestSanitizeEWMA_ScrubsPoisonedPointsOnLoad(t *testing.T) {
	now := time.Now()
	in := map[string]*nodeEWMA{
		// Poisoned: decayed-sentinel values (what 92's state file held).
		"sub/poisoned": {
			Short:       ewmaPoint{Value: 1535756.3, UpdatedAt: now},
			Long:        ewmaPoint{Value: 336410256.9, UpdatedAt: now},
			FirstSeenAt: now.Add(-38 * time.Hour),
		},
		// Clean: a real composite — must survive untouched.
		"sub/clean": {
			Short:       ewmaPoint{Value: 249, UpdatedAt: now},
			Long:        ewmaPoint{Value: 251, UpdatedAt: now},
			FirstSeenAt: now.Add(-38 * time.Hour),
		},
		// Residual: a short-window sentinel that has decayed to ~3.2e5 —
		// below the original 1e6 floor (so it slipped through and left a
		// reloaded node's short EWMA poisoned on 92), but still ~13x above
		// any real composite. The lowered poisonFloor (5e4) must catch it.
		"sub/residual": {
			Short:       ewmaPoint{Value: 321267, UpdatedAt: now},
			Long:        ewmaPoint{Value: 5000, UpdatedAt: now}, // real — keep
			FirstSeenAt: now.Add(-38 * time.Hour),
		},
	}
	out := copyEWMA(in)

	p := out["sub/poisoned"]
	if !p.Short.UpdatedAt.IsZero() || p.Short.Value != 0 {
		t.Errorf("poisoned short point not scrubbed: %+v", p.Short)
	}
	if !p.Long.UpdatedAt.IsZero() || p.Long.Value != 0 {
		t.Errorf("poisoned long point not scrubbed: %+v", p.Long)
	}
	if p.FirstSeenAt.IsZero() {
		t.Error("scrub must preserve FirstSeenAt (trial window), only zero the values")
	}

	r := out["sub/residual"]
	if !r.Short.UpdatedAt.IsZero() || r.Short.Value != 0 {
		t.Errorf("residual short sentinel (~3.2e5) not scrubbed by lowered floor: %+v", r.Short)
	}
	if r.Long.Value != 5000 || r.Long.UpdatedAt.IsZero() {
		t.Errorf("residual node's real long value wrongly scrubbed: %+v", r.Long)
	}

	c := out["sub/clean"]
	if c.Long.Value != 251 || c.Long.UpdatedAt.IsZero() {
		t.Errorf("clean long point wrongly scrubbed: %+v", c.Long)
	}
}





// TestPoolSetSurvivesSaveLoad verifies the pool membership persists across a
// restart (save → new Scorer → load), so anti-flap hysteresis state is not
// lost. Without this the reloaded scorer starts with an empty poolSet and the
// next round runs the unguarded bootstrap path.
func TestPoolSetSurvivesSaveLoad(t *testing.T) {
	dir := t.TempDir()
	s := newTestScorer(defaultCfg())
	s.state = map[string]*nodeState{}
	s.renderer = &emergencyTestRenderer{path: dir + "/cfg.yaml"}
	s.poolSet = map[string]bool{"nodeA": true, "nodeB": true, "nodeC": true}

	s.mu.Lock()
	s.saveStateLocked()
	s.mu.Unlock()

	// Fresh scorer pointed at the same state file (same renderer path).
	s2 := newTestScorer(defaultCfg())
	s2.state = map[string]*nodeState{}
	s2.renderer = &emergencyTestRenderer{path: dir + "/cfg.yaml"}
	s2.loadState()

	if len(s2.poolSet) != 3 {
		t.Fatalf("poolSet size after reload = %d, want 3 (%v)", len(s2.poolSet), s2.poolSet)
	}
	for _, tag := range []string{"nodeA", "nodeB", "nodeC"} {
		if !s2.poolSet[tag] {
			t.Errorf("poolSet missing %q after reload", tag)
		}
	}
}

// TestPoolSetColdStartWhenAbsent verifies a state file with no pool_set key
// (written before the field existed) reloads to an empty poolSet — the
// cold-start path — rather than crashing or inventing membership.
func TestPoolSetColdStartWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	// Hand-write a v3 file lacking the pool_set key.
	legacy := `{"version":3,"nodes":{"nodeA":{"strikes":5,"ok_rounds":2}},"effective_pool":["nodeA"]}`
	if err := os.WriteFile(dir+"/nodescorer-state.json", []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestScorer(defaultCfg())
	s.state = map[string]*nodeState{}
	s.renderer = &emergencyTestRenderer{path: dir + "/cfg.yaml"}
	s.loadState()

	if len(s.poolSet) != 0 {
		t.Fatalf("poolSet after loading pool_set-less file = %d, want 0 (cold start)", len(s.poolSet))
	}
	// Sanity: the rest of v3 still loaded.
	if s.state["nodeA"] == nil || s.state["nodeA"].strikes != 5 {
		t.Errorf("v3 node state not restored alongside cold-start poolSet")
	}
}

// eligibilityCfg returns an auto-mode config in sizing_mode=eligibility.
// K-gating params are set so that legacy mode WOULD cut to 8 (kDemand=8),
// proving the eligibility path ignores the target and keeps all qualified.
func eligibilityCfg() config.NodeQualifyConfig {
	cfg := kGatingCfg(8) // legacy would target 8
	cfg.SizingMode = "eligibility"
	cfg.PoolSizing.MinPoolSize = 3
	return cfg
}

// TestEligibility_KeepsAllQualifiedNoCut: 9 qualified nodes must ALL be in
// the pool. Legacy K-gating (kDemand=8) would cut one; eligibility must not
// — there is no fixed target, so the marginal-slot contest cannot occur.
func TestEligibility_KeepsAllQualifiedNoCut(t *testing.T) {
	cfg := eligibilityCfg()
	nodes := map[string]proxyNode{}
	for i, rtt := range []int{100, 110, 120, 130, 140, 150, 160, 170, 180} {
		nodes[fmt.Sprintf("sub/n%d", i+1)] = proxyNode{alive: true, history: goodHistory(rtt)}
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes)
	// Simulate an established running pool (as T0 poolSet-restore provides
	// after a restart): all 9 are currently in-pool, so the trial filter —
	// which correctly keeps a brand-new node out until it proves 24h of
	// stability — does not apply to them. This isolates the property under
	// test: eligibility does NOT cut an established qualified set to a K.
	s.poolSet = map[string]bool{}
	for name := range nodes {
		s.poolSet[name] = true
	}
	s.score(context.Background())

	pool := inPoolNames(s.GetSnapshot())
	if len(pool) != 9 {
		t.Fatalf("eligibility pool = %d, want all 9 qualified (legacy would cut to 8); pool=%v", len(pool), pool)
	}
}

// TestEligibility_FloorFillWhenBelowMin: only 2 nodes qualify; the pool must
// be filled to min_pool_size=3 from the best available, so per-terminal HRW
// top-2 never degrades to a single-egress-violating bare MATCH,us-pool.
func TestEligibility_FloorFillWhenBelowMin(t *testing.T) {
	cfg := eligibilityCfg()
	// 2 clearly-good nodes + 1 mediocre-but-alive node available to fill.
	nodes := map[string]proxyNode{
		"sub/good1": {alive: true, history: goodHistory(100)},
		"sub/good2": {alive: true, history: goodHistory(120)},
		"sub/fill":  {alive: true, history: goodHistory(400)},
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes)
	s.score(context.Background())

	pool := inPoolNames(s.GetSnapshot())
	if len(pool) < 3 {
		t.Fatalf("pool = %d, want >= min_pool_size 3 (floor fill); pool=%v", len(pool), pool)
	}
}

// TestEligibility_FloorFillPrefersIncumbent locks in T3 fill-stability: when
// the pool is floor-bound and must admit a filler, an incumbent filler (one
// already in poolSet) is kept even if a non-incumbent candidate has a better
// EWMA. Without incumbency the fill re-sorts on EWMA noise every round and
// the floor-filler ping-pongs — the exact mini-flap the guard review flagged.
func TestEligibility_FloorFillPrefersIncumbent(t *testing.T) {
	cfg := eligibilityCfg() // min_pool_size = 3
	// 2 good qualified nodes + 2 fill candidates. "incumbent" has a WORSE
	// (higher) latency than "challenger", so a pure EWMA sort would pick the
	// challenger. Incumbency must override and keep the incumbent.
	nodes := map[string]proxyNode{
		"sub/good1":      {alive: true, history: goodHistory(100)},
		"sub/good2":      {alive: true, history: goodHistory(120)},
		"sub/incumbent":  {alive: true, history: goodHistory(500)},
		"sub/challenger": {alive: true, history: goodHistory(300)},
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes)
	// good1/good2 established in-pool; incumbent is the current floor-filler.
	// All three must be past trial (in poolSet) so eligibility keeps them;
	// challenger is fresh/out so it can only enter via the fill sort.
	s.poolSet = map[string]bool{"sub/good1": true, "sub/good2": true, "sub/incumbent": true}
	s.score(context.Background())

	pool := inPoolNames(s.GetSnapshot())
	if len(pool) != 3 {
		t.Fatalf("pool = %d, want exactly min_pool 3; pool=%v", len(pool), pool)
	}
	if !pool["sub/incumbent"] {
		t.Errorf("incumbent filler dropped despite incumbency; pool=%v", pool)
	}
	if pool["sub/challenger"] {
		t.Errorf("challenger admitted over incumbent (EWMA sort won, incumbency lost); pool=%v", pool)
	}
}

// TestShadowMode_DoesNotChangeLivePool verifies sizing_shadow logs what
// eligibility WOULD do but leaves the live legacy decision untouched. With
// 9 qualified and legacy kDemand=8, the live pool must still be 8 (legacy),
// even though eligibility would keep 9 — the diff is logged, not applied.
func TestShadowMode_DoesNotChangeLivePool(t *testing.T) {
	cfg := kGatingCfg(8) // legacy target 8
	cfg.SizingMode = "legacy"
	cfg.SizingShadow = true
	cfg.PoolSizing.MinPoolSize = 3
	nodes := map[string]proxyNode{}
	for i, rtt := range []int{100, 110, 120, 130, 140, 150, 160, 170, 180} {
		nodes[fmt.Sprintf("sub/n%d", i+1)] = proxyNode{alive: true, history: goodHistory(rtt)}
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes)
	// Establish all 9 in-pool so neither path is throttled by trial/bootstrap.
	s.poolSet = map[string]bool{}
	for name := range nodes {
		s.poolSet[name] = true
	}
	s.score(context.Background())

	pool := inPoolNames(s.GetSnapshot())
	k := s.GetSnapshot().PoolSizing.KTarget
	// Live pool must follow LEGACY (K-gated), not eligibility.
	if len(pool) != k {
		t.Fatalf("shadow perturbed live pool: size=%d, legacy K=%d; pool=%v", len(pool), k, pool)
	}
	if len(pool) == 9 {
		t.Errorf("live pool = 9 → shadow leaked into the live decision (should be legacy K=%d)", k)
	}
}
