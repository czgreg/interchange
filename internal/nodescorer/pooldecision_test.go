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
// CHARACTERIZATION FINDING: a catastrophic member is evicted on the NEXT
// round whenever a healthy spare can backfill, via the exact-K post-pass
// ("Post-pass — enforce |pool| == kTarget"). When n4 goes catastrophic it
// drops from preferredSet; the
// top-K qualified set then promotes the spare, pushing |newPoolSet| over
// K; the over-capacity pass drops non-preferred members worst-first — and
// n4, being unqualified, is exactly such a member. Net effect: a
// catastrophic member with a healthy replacement is evicted on the NEXT
// round (strikes=1), not after EvictStrikes rounds.
//
// This is arguably the right behavior (don't keep a dead node when a good
// one is ready). Note what is NOT doing the work here: cfg.EvictStrikes is
// set below but no path reads it — flipping it to 1 leaves this test and
// TestDecision_HysteresisHoldsWithoutSpare both green (checked
// 2026-09-10). The delay in that other test comes from the K-floor refill,
// not from strikes. See config.EvictStrikes' field doc.
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

// --- A pool member that drops out of the preferred top-K stays in the
// pool when there is NO spare, because evicting it would drop the pool
// below K and the under-capacity pass would just re-add it.
//
// The K-floor refill is the whole mechanism; EvictStrikes is set below but
// is not read by any path (flipping it to 1 keeps this test green), so
// read this as "the floor holds the member", not as eviction hysteresis.
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
	cfg := eligibilityCfg() // min_pool_size = 3, max_rtt_p95_ms = 800
	// 2 good qualified nodes (admitted on merit) + 2 fill-only candidates.
	//
	// Both fillers sit ABOVE max_rtt_p95_ms so the admission quality gate
	// refuses them, leaving the pool at 2 < min_pool 3 and forcing the floor
	// fill to choose between them. They are still Qualified (no failed probes,
	// recentOk=10), so they remain eligible for the floor fill, which applies
	// no quality ceiling by design: below the floor a slow node beats no node.
	//
	// "incumbent" is SLOWER than "challenger", so a pure EWMA sort would pick
	// the challenger. Incumbency must override and keep the incumbent.
	//
	// Before the admission gate replaced the trial window, this fixture used
	// 500/300ms fillers held out by inTrial. Both now pass the quality gate on
	// merit, which would make the pool 4 and never reach the floor path this
	// test exists to cover — hence the above-threshold latencies.
	nodes := map[string]proxyNode{
		"sub/good1":      {alive: true, history: goodHistory(100)},
		"sub/good2":      {alive: true, history: goodHistory(120)},
		"sub/incumbent":  {alive: true, history: goodHistory(900)},
		"sub/challenger": {alive: true, history: goodHistory(850)},
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes)
	// good1/good2/incumbent established in-pool (as poolSet-restore provides
	// after a restart); incumbent is the current floor-filler. Challenger is
	// out, so it can only enter via the fill sort.
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

// TestEligibility_O1_FlapBackNodeExcluded: a non-incumbent alive node with
// no probe history (ProbeCount==0, Qualified via benefit-of-doubt) must NOT
// be admitted to routing — this is the flapping-dead-node case (US-01..05
// flicker alive, get admitted, reveal fail=1.0, drop). It stays out until it
// earns ProbeCount>0.
func TestEligibility_O1_FlapBackNodeExcluded(t *testing.T) {
	cfg := eligibilityCfg()
	nodes := map[string]proxyNode{
		"sub/good1":   {alive: true, history: goodHistory(100)},
		"sub/good2":   {alive: true, history: goodHistory(120)},
		"sub/good3":   {alive: true, history: goodHistory(140)},
		"sub/flapper": {alive: true, history: []int{}}, // ProbeCount==0, just flickered alive
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes)
	// good1-3 established in pool; flapper is NOT (it just came back).
	s.poolSet = map[string]bool{"sub/good1": true, "sub/good2": true, "sub/good3": true}
	s.score(context.Background())

	pool := inPoolNames(s.GetSnapshot())
	if pool["sub/flapper"] {
		t.Errorf("flap-back node (ProbeCount==0, non-incumbent) was admitted; pool=%v", pool)
	}
	if len(pool) != 3 {
		t.Errorf("pool=%d want 3 (the 3 measured good nodes); pool=%v", len(pool), pool)
	}
}

// TestEligibility_O1_IncumbentNoHistoryKept: an INCUMBENT with ProbeCount==0
// (e.g. url-test history wiped by a hot-reload/restart, but it was measured-
// good before and restored via poolSet) must be KEPT through the rebuild
// window — otherwise every hot-reload would drain the pool.
func TestEligibility_O1_IncumbentNoHistoryKept(t *testing.T) {
	cfg := eligibilityCfg()
	nodes := map[string]proxyNode{
		"sub/inc1": {alive: true, history: []int{}}, // incumbent, history rebuilding
		"sub/inc2": {alive: true, history: []int{}},
		"sub/inc3": {alive: true, history: []int{}},
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes)
	s.poolSet = map[string]bool{"sub/inc1": true, "sub/inc2": true, "sub/inc3": true}
	s.score(context.Background())

	pool := inPoolNames(s.GetSnapshot())
	for _, n := range []string{"sub/inc1", "sub/inc2", "sub/inc3"} {
		if !pool[n] {
			t.Errorf("incumbent %s (ProbeCount==0, rebuild window) was dropped; pool=%v", n, pool)
		}
	}
}

// TestEligibility_O1_ColdStartFillsFloor: genuine first boot — empty poolSet,
// every node ProbeCount==0. The cold-start last-resort must still bring the
// pool up to min_pool_size so the data plane has a us-pool.
func TestEligibility_O1_ColdStartFillsFloor(t *testing.T) {
	cfg := eligibilityCfg() // min_pool_size=3
	nodes := map[string]proxyNode{
		"sub/n1": {alive: true, history: []int{}},
		"sub/n2": {alive: true, history: []int{}},
		"sub/n3": {alive: true, history: []int{}},
		"sub/n4": {alive: true, history: []int{}},
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes) // poolSet empty (fresh)
	s.score(context.Background())

	pool := inPoolNames(s.GetSnapshot())
	if len(pool) < 3 {
		t.Fatalf("cold start pool=%d, want >= min_pool 3 (data plane must not be starved); pool=%v", len(pool), pool)
	}
}

// TestEligibility_O1_ColdStartRespectsQuarantine: the cold-start availability
// fill bypasses TRIAL but must NOT bypass QUARANTINE. A node the operator
// rolled back (rollbackQuarantine) must never be re-admitted by a
// restart-into-low-pool, even if that leaves the pool below min_pool_size —
// a banned node routing traffic is worse than a degraded pool.
func TestEligibility_O1_ColdStartRespectsQuarantine(t *testing.T) {
	cfg := eligibilityCfg() // min_pool_size=3
	nodes := map[string]proxyNode{
		"sub/banned": {alive: true, history: []int{}}, // quarantined, pc=0
		"sub/g1":     {alive: true, history: []int{}}, // cold, pc=0
		"sub/g2":     {alive: true, history: []int{}},
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes) // empty poolSet (cold)
	s.rollbackQuarantine = map[string]time.Time{"sub/banned": time.Now().Add(1 * time.Hour)}
	s.score(context.Background())

	pool := inPoolNames(s.GetSnapshot())
	if pool["sub/banned"] {
		t.Errorf("quarantined node re-admitted by cold-start fill — rollback defeated; pool=%v", pool)
	}
}

// softDeadHistory: alive=false but history has successes → ProbeCount>0,
// recentOk>0 → Qualified=true. The soft-dead-incumbent case.
func softDeadNode() proxyNode { return proxyNode{alive: false, history: goodHistory(300)} }

// TestDeadEvict_EvictedAfterThresholdWhenAboveFloor: a soft-dead in-pool
// node (alive=false, still Qualified) is evicted after DeadEvictRounds
// consecutive rounds, since enough alive nodes remain above min_pool.
func TestDeadEvict_EvictedAfterThresholdWhenAboveFloor(t *testing.T) {
	cfg := eligibilityCfg() // min_pool=3
	cfg.DeadEvictRounds = 2
	nodes := map[string]proxyNode{
		"sub/g1": {alive: true, history: goodHistory(100)},
		"sub/g2": {alive: true, history: goodHistory(110)},
		"sub/g3": {alive: true, history: goodHistory(120)},
		"sub/g4": {alive: true, history: goodHistory(130)},
		"sub/softdead": softDeadNode(),
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes)
	s.poolSet = map[string]bool{"sub/g1": true, "sub/g2": true, "sub/g3": true, "sub/g4": true, "sub/softdead": true}
	ctx := context.Background()
	s.score(ctx) // round 1: deadRounds=1, still in
	if !inPoolNames(s.GetSnapshot())["sub/softdead"] {
		t.Fatal("soft-dead should still be in pool at round 1 (debounce not reached)")
	}
	s.score(ctx) // round 2: deadRounds=2 >= threshold → evicted
	pool := inPoolNames(s.GetSnapshot())
	if pool["sub/softdead"] {
		t.Errorf("soft-dead not evicted after DeadEvictRounds=2; pool=%v", pool)
	}
	if len(pool) != 4 {
		t.Errorf("pool=%d want 4 alive (soft-dead gone, above floor); pool=%v", len(pool), pool)
	}
}

// TestDeadEvict_RetainedToHoldFloor: when evicting soft-dead nodes would
// drop the pool below min_pool_size, they are retained (floor-subordinate).
func TestDeadEvict_RetainedToHoldFloor(t *testing.T) {
	cfg := eligibilityCfg() // min_pool=3
	cfg.DeadEvictRounds = 2
	nodes := map[string]proxyNode{
		"sub/g1":  {alive: true, history: goodHistory(100)},
		"sub/g2":  {alive: true, history: goodHistory(110)},
		"sub/sd1": softDeadNode(),
		"sub/sd2": softDeadNode(),
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes)
	s.poolSet = map[string]bool{"sub/g1": true, "sub/g2": true, "sub/sd1": true, "sub/sd2": true}
	ctx := context.Background()
	s.score(ctx); s.score(ctx) // reach threshold
	pool := inPoolNames(s.GetSnapshot())
	if len(pool) < 3 {
		t.Fatalf("pool=%d below min_pool 3 — floor not held with dead nodes; pool=%v", len(pool), pool)
	}
	// exactly 1 soft-dead retained to reach floor of 3 (2 alive + 1 dead)
	sd := (pool["sub/sd1"]) != (pool["sub/sd2"]) || pool["sub/sd1"] && pool["sub/sd2"]
	if !sd {
		t.Errorf("expected soft-dead retained to hold floor; pool=%v", pool)
	}
}

// TestDeadEvict_CounterResetsOnAlive: a node that goes alive again before
// the threshold resets its deadRounds and is never evicted.
func TestDeadEvict_CounterResetsOnAlive(t *testing.T) {
	cfg := eligibilityCfg()
	cfg.DeadEvictRounds = 3
	nodes := map[string]proxyNode{
		"sub/g1":   {alive: true, history: goodHistory(100)},
		"sub/g2":   {alive: true, history: goodHistory(110)},
		"sub/g3":   {alive: true, history: goodHistory(120)},
		"sub/flap": softDeadNode(),
	}
	s, _, mutate := newDecisionScorer(t, cfg, nodes)
	s.poolSet = map[string]bool{"sub/g1": true, "sub/g2": true, "sub/g3": true, "sub/flap": true}
	ctx := context.Background()
	s.score(ctx); s.score(ctx) // deadRounds=2 (below threshold 3)
	mutate("sub/flap", goodHistory(150)) // now alive=true → should reset
	s.score(ctx) // deadRounds resets to 0
	s.score(ctx) // stays 0
	if !inPoolNames(s.GetSnapshot())["sub/flap"] {
		t.Errorf("recovered node evicted despite alive reset before threshold")
	}
}

// --- Admission quality gate (docs/design-admission-quality-gate.md) ---------
//
// These lock in the replacement of the 24h trial window with a one-time
// absolute quality bar for NEW admissions. The core promise is asymmetric:
// a good new node is usable within a scoring round or two, a mid-grade bad
// node is refused indefinitely, and incumbents are never re-judged on quality
// (re-judging them is the Plan-A churn eligibility mode exists to remove).

// jitteryHistory returns a 10-probe history alternating lo/hi so p95-p50
// produces a large jitter while every probe still succeeds (fail_rate=0).
// Used to exercise the MaxJitterMs axis in isolation.
//
// With 5 lo + 5 hi entries, percentile() interpolates p50 to (lo+hi)/2 and
// p95 to hi, so jitter = (hi-lo)/2. To exceed max_jitter_ms=300 while staying
// under max_rtt_p95_ms=800 the spread must be > 600 with hi <= 800.
func jitteryHistory(lo, hi int) []int {
	h := make([]int, 10)
	for i := range h {
		if i%2 == 0 {
			h[i] = lo
		} else {
			h[i] = hi
		}
	}
	return h
}

// partialFailHistory returns a history with `failures` zero-entries out of 10
// (fail_rate = failures/10), the rest at rttMs. Stays under the catastrophic
// 0.9 gate so the node is still Qualified — which is exactly the hole the
// admission gate closes.
func partialFailHistory(failures, rttMs int) []int {
	h := make([]int, 10)
	for i := range h {
		if i < failures {
			h[i] = 0
		} else {
			h[i] = rttMs
		}
	}
	return h
}

// TestAdmission_GoodNewNodeAdmittedImmediately: the headline behavior. A brand
// new node (no EWMA history, so inTrial would have held it out for 24h) that
// measures well is admitted on the first scored round. No poolSet seeding —
// this is a genuine newcomer.
func TestAdmission_GoodNewNodeAdmittedImmediately(t *testing.T) {
	cfg := eligibilityCfg()
	nodes := map[string]proxyNode{
		"sub/inc1":     {alive: true, history: goodHistory(100)},
		"sub/inc2":     {alive: true, history: goodHistory(110)},
		"sub/inc3":     {alive: true, history: goodHistory(120)},
		"sub/newcomer": {alive: true, history: goodHistory(250)}, // well under 800
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes)
	s.poolSet = map[string]bool{"sub/inc1": true, "sub/inc2": true, "sub/inc3": true}
	s.score(context.Background())

	pool := inPoolNames(s.GetSnapshot())
	if !pool["sub/newcomer"] {
		t.Errorf("good new node NOT admitted on first round (the 24h-trial regression); pool=%v", pool)
	}
	if s.inTrial("sub/newcomer", time.Now()) != true {
		t.Errorf("fixture invalid: newcomer should still be inTrial, "+
			"otherwise this test would pass even with the old gate")
	}
}

// TestAdmission_RefusesSlowNewNode: a node that is alive and Qualified but
// exceeds max_rtt_p95_ms must NOT be admitted. Under the old trial gate this
// node was admitted 24h later; it must now be refused while the pool is
// healthy (above the floor, so the floor bypass does not rescue it).
func TestAdmission_RefusesSlowNewNode(t *testing.T) {
	cfg := eligibilityCfg()
	nodes := map[string]proxyNode{
		"sub/inc1": {alive: true, history: goodHistory(100)},
		"sub/inc2": {alive: true, history: goodHistory(110)},
		"sub/inc3": {alive: true, history: goodHistory(120)},
		"sub/slow": {alive: true, history: goodHistory(3000)}, // p95 3000 > 800
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes)
	s.poolSet = map[string]bool{"sub/inc1": true, "sub/inc2": true, "sub/inc3": true}
	s.score(context.Background())

	pool := inPoolNames(s.GetSnapshot())
	if pool["sub/slow"] {
		t.Errorf("slow node admitted despite p95 3000 > max_rtt_p95_ms 800; pool=%v", pool)
	}
	if len(pool) != 3 {
		t.Errorf("pool = %d, want 3 (the 3 incumbents only); pool=%v", len(pool), pool)
	}
}

// TestAdmission_RefusesMidGradeFailingNode: fail_rate 0.5 is BELOW the
// catastrophic 0.9 gate, so scoreNode still marks it Qualified. This is the
// exact hole the 24h trial was accidentally covering: previously admitted a
// day later, now refused outright.
func TestAdmission_RefusesMidGradeFailingNode(t *testing.T) {
	cfg := eligibilityCfg()
	nodes := map[string]proxyNode{
		"sub/inc1": {alive: true, history: goodHistory(100)},
		"sub/inc2": {alive: true, history: goodHistory(110)},
		"sub/inc3": {alive: true, history: goodHistory(120)},
		"sub/mid":  {alive: true, history: partialFailHistory(5, 200)}, // fail 0.5 > 0.25
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes)
	s.poolSet = map[string]bool{"sub/inc1": true, "sub/inc2": true, "sub/inc3": true}
	s.score(context.Background())

	var mid *NodeHealth
	for _, n := range s.GetSnapshot().Nodes {
		if n.Name == "sub/mid" {
			h := n
			mid = &h
		}
	}
	if mid == nil {
		t.Fatal("sub/mid missing from snapshot")
	}
	if !mid.Qualified {
		t.Fatalf("fixture invalid: fail=0.5 must stay Qualified (below the 0.9 "+
			"catastrophic gate) or this test proves nothing; reason=%q", mid.Reason)
	}
	if inPoolNames(s.GetSnapshot())["sub/mid"] {
		t.Errorf("fail_rate 0.5 node admitted despite max_fail_rate 0.25")
	}
}

// TestAdmission_RefusesJitteryNewNode covers the MaxJitterMs axis on its own:
// every probe succeeds and p95 stays under the ceiling, but the spread does
// not. A high-jitter egress is felt as intermittent stalls by whichever
// terminal HRW pins to it.
func TestAdmission_RefusesJitteryNewNode(t *testing.T) {
	cfg := eligibilityCfg()
	nodes := map[string]proxyNode{
		"sub/inc1":    {alive: true, history: goodHistory(100)},
		"sub/inc2":    {alive: true, history: goodHistory(110)},
		"sub/inc3":    {alive: true, history: goodHistory(120)},
		"sub/jittery": {alive: true, history: jitteryHistory(50, 750)}, // jitter 350 > 300, p95 750 < 800
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes)
	s.poolSet = map[string]bool{"sub/inc1": true, "sub/inc2": true, "sub/inc3": true}
	s.score(context.Background())

	if inPoolNames(s.GetSnapshot())["sub/jittery"] {
		t.Errorf("jittery node admitted despite jitter > max_jitter_ms 300")
	}
}

// TestAdmission_IncumbentExemptFromQualityGate: the one-way door. An incumbent
// that degrades past every threshold must STAY in the pool — gating incumbents
// on quality re-introduces per-round membership flipping. This is also the
// honest statement of the gate's limitation (ops doc U1 is not solved here).
func TestAdmission_IncumbentExemptFromQualityGate(t *testing.T) {
	cfg := eligibilityCfg()
	nodes := map[string]proxyNode{
		"sub/inc1":     {alive: true, history: goodHistory(100)},
		"sub/inc2":     {alive: true, history: goodHistory(110)},
		"sub/inc3":     {alive: true, history: goodHistory(120)},
		"sub/degraded": {alive: true, history: goodHistory(4000)}, // way over 800
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes)
	// degraded is ALREADY carrying traffic.
	s.poolSet = map[string]bool{
		"sub/inc1": true, "sub/inc2": true, "sub/inc3": true, "sub/degraded": true,
	}
	s.score(context.Background())

	if !inPoolNames(s.GetSnapshot())["sub/degraded"] {
		t.Errorf("incumbent evicted by the admission gate — the gate must be " +
			"one-way (entry only), or it re-introduces Plan-A churn")
	}
}

// TestAdmission_FloorBypassIgnoresQualityGate: availability outranks quality
// below min_pool_size. With only 1 good node and min_pool 3, the floor fill
// must still admit refused-on-quality nodes — below the floor per-terminal HRW
// top-2 degrades and breaks the single-egress invariant, so a slow node beats
// no node.
func TestAdmission_FloorBypassIgnoresQualityGate(t *testing.T) {
	cfg := eligibilityCfg() // min_pool_size = 3
	nodes := map[string]proxyNode{
		"sub/good":  {alive: true, history: goodHistory(100)},
		"sub/slow1": {alive: true, history: goodHistory(2000)}, // refused on quality
		"sub/slow2": {alive: true, history: goodHistory(2500)}, // refused on quality
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes)
	s.score(context.Background())

	pool := inPoolNames(s.GetSnapshot())
	if len(pool) < 3 {
		t.Errorf("pool = %d, want >= min_pool_size 3: the floor bypass must "+
			"override the quality gate (availability > quality below floor); pool=%v",
			len(pool), pool)
	}
}

// TestAdmission_UnmeasuredNewNodeRefused: preserves the O1 measured-history
// rule through the rewrite. A node whose alive bit just flickered on has
// ProbeCount 0 and Qualified=true ("benefit of doubt"); it must not carry
// users before a single real measurement. MinProbes subsumes the former
// ProbeCount>0 condition.
func TestAdmission_UnmeasuredNewNodeRefused(t *testing.T) {
	cfg := eligibilityCfg()
	nodes := map[string]proxyNode{
		"sub/inc1":  {alive: true, history: goodHistory(100)},
		"sub/inc2":  {alive: true, history: goodHistory(110)},
		"sub/inc3":  {alive: true, history: goodHistory(120)},
		"sub/fresh": {alive: true, history: nil}, // ProbeCount 0
	}
	s, _, _ := newDecisionScorer(t, cfg, nodes)
	s.poolSet = map[string]bool{"sub/inc1": true, "sub/inc2": true, "sub/inc3": true}
	s.score(context.Background())

	if inPoolNames(s.GetSnapshot())["sub/fresh"] {
		t.Errorf("node with no probe history admitted (O1 regression)")
	}
}

// --- Candidate discovery (docs/ops-candidate-discovery-loop.md) -------------
//
// Before discovery, score() built its candidate set only from us-pool.all ∪
// (state ∩ /proxies), while us-pool's membership was written by the renderer
// from the scorer's own output — a closed loop with no entry point for a node
// the scorer had never seen. A newly-added subscription was therefore invisible
// forever, restart included. These tests pin the entry point open.

// mockClashAPIPartialPool is mockClashAPI with control over which nodes appear
// in us-pool.all. The default helper puts EVERY node in us-pool, which is
// exactly the state the discovery bug cannot occur in — so the loop was
// untestable with it. inPool lists the us-pool.all members; every node in
// `nodes` is still present in /proxies, mirroring production (the renderer had
// written all 179 proxies while us-pool held only 13).
func mockClashAPIPartialPool(t *testing.T, probeURL string, nodes map[string]proxyNode, inPool []string) (addr string, mutate func(tag string, history []int)) {
	t.Helper()
	var mu sync.Mutex
	proxies := map[string]map[string]interface{}{}
	for tag, n := range nodes {
		proxies[tag] = buildProxyEntry(n.alive, probeURL, n.history)
	}
	all := make([]interface{}, 0, len(inPool))
	for _, tag := range inPool {
		all = append(all, tag)
	}
	proxies["us-pool"] = map[string]interface{}{
		"type": "LoadBalance", "all": all, "now": "",
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
	return strings.TrimPrefix(srv.URL, "http://"), mutate
}

// outbound builds a minimal subscription outbound carrying just a tag, which
// is all filterCandidates reads.
func outbound(tag string) subscribe.Outbound {
	return subscribe.Outbound{"tag": tag, "type": "vless"}
}

// newDiscoveryScorer wires a scorer whose /proxies contains every node but
// whose us-pool contains only `inPool`, with `subs` as the subscription
// outbounds and a real node pattern.
func newDiscoveryScorer(t *testing.T, cfg config.NodeQualifyConfig, nodes map[string]proxyNode,
	inPool []string, subs []string, pattern string) (*Scorer, *fakeRenderer) {
	t.Helper()
	const probeURL = "http://probe.test/generate_204"
	apiAddr, _ := mockClashAPIPartialPool(t, probeURL, nodes, inPool)
	fr := &fakeRenderer{path: t.TempDir() + "/config.yaml"}
	obs := make([]subscribe.Outbound, 0, len(subs))
	for _, tag := range subs {
		obs = append(obs, outbound(tag))
	}
	s := New(cfg, nil, apiAddr, "", probeURL, pattern, fr,
		func() []subscribe.Outbound { return obs })
	t.Cleanup(func() { time.Sleep(50 * time.Millisecond) })
	return s, fr
}

// TestDiscovery_NewSubscriptionNodeBecomesCandidate: the headline bug. A node
// present in /proxies and in AllOutbounds, matching node_pattern, but absent
// from us-pool.all and from state, must still be scored.
func TestDiscovery_NewSubscriptionNodeBecomesCandidate(t *testing.T) {
	cfg := eligibilityCfg()
	nodes := map[string]proxyNode{
		"ash/US-01":      {alive: true, history: goodHistory(100)},
		"ash/US-02":      {alive: true, history: goodHistory(110)},
		"ash/US-03":      {alive: true, history: goodHistory(120)},
		"ash/US-04":      {alive: true, history: goodHistory(130)},
		"soonlink/US-新": {alive: true, history: goodHistory(250)},
		"soonlink/HK-01": {alive: true, history: goodHistory(80)}, // must NOT match
	}
	inPool := []string{"ash/US-01", "ash/US-02", "ash/US-03", "ash/US-04"}
	subs := []string{"ash/US-01", "ash/US-02", "ash/US-03", "ash/US-04",
		"soonlink/US-新", "soonlink/HK-01"}
	s, _ := newDiscoveryScorer(t, cfg, nodes, inPool, subs, `US`)
	s.poolSet = map[string]bool{
		"ash/US-01": true, "ash/US-02": true, "ash/US-03": true, "ash/US-04": true,
	}
	s.score(context.Background())

	seen := map[string]bool{}
	for _, n := range s.GetSnapshot().Nodes {
		seen[n.Name] = true
	}
	if !seen["soonlink/US-新"] {
		t.Errorf("discovery failed: new subscription node never scored; seen=%v", seen)
	}
	if seen["soonlink/HK-01"] {
		t.Errorf("node_pattern ignored: non-matching node became a candidate; seen=%v", seen)
	}
}

// TestDiscovery_TriggersRenderEvenWhenRoutingSetUnchanged is BLOCKER 1. The
// discovered node cannot enter the ROUTING set yet (ProbeCount==0 → refused by
// the admission gate), so poolChanged is false. If the reload trigger only
// watched the routing set, no render would fire, the node would never reach
// mihomo's us-pool, would never be url-tested, and would be refused forever
// while the logs looked healthy. The probing set changing must itself trigger.
func TestDiscovery_TriggersRenderEvenWhenRoutingSetUnchanged(t *testing.T) {
	cfg := eligibilityCfg()
	cfg.HotReloadMinInterval = 0 // don't let the throttle mask the trigger
	nodes := map[string]proxyNode{
		"ash/US-01":     {alive: true, history: goodHistory(100)},
		"ash/US-02":     {alive: true, history: goodHistory(110)},
		"ash/US-03":     {alive: true, history: goodHistory(120)},
		"ash/US-04":     {alive: true, history: goodHistory(130)},
		"soonlink/US-新": {alive: true, history: nil}, // unmeasured: cannot route yet
	}
	inPool := []string{"ash/US-01", "ash/US-02", "ash/US-03", "ash/US-04"}
	subs := append(append([]string{}, inPool...), "soonlink/US-新")
	s, fr := newDiscoveryScorer(t, cfg, nodes, inPool, subs, `US`)
	s.poolSet = map[string]bool{
		"ash/US-01": true, "ash/US-02": true, "ash/US-03": true, "ash/US-04": true,
	}
	s.score(context.Background())
	fr.waitRenders(t, 1)

	usPool, routing, _ := fr.snapshot()
	if !contains(usPool, "soonlink/US-新") {
		t.Errorf("BLOCKER 1: discovered node absent from rendered us-pool, so mihomo "+
			"would never probe it; us_pool=%v", usPool)
	}
	if contains(routing, "soonlink/US-新") {
		t.Errorf("unmeasured node placed in routing set; routing=%v", routing)
	}
}

// TestDiscovery_IdempotentNoRenderStorm: once the discovered node is in the
// rendered us-pool, an unchanged candidate set must NOT keep re-rendering.
// Comparing against our own last intent (rather than against mihomo's current
// us-pool, which is intersected with AllOutbounds) is what makes this hold.
func TestDiscovery_IdempotentNoRenderStorm(t *testing.T) {
	cfg := eligibilityCfg()
	cfg.HotReloadMinInterval = 0
	nodes := map[string]proxyNode{
		"ash/US-01":     {alive: true, history: goodHistory(100)},
		"ash/US-02":     {alive: true, history: goodHistory(110)},
		"ash/US-03":     {alive: true, history: goodHistory(120)},
		"ash/US-04":     {alive: true, history: goodHistory(130)},
		"soonlink/US-新": {alive: true, history: goodHistory(250)},
	}
	inPool := []string{"ash/US-01", "ash/US-02", "ash/US-03", "ash/US-04"}
	subs := append(append([]string{}, inPool...), "soonlink/US-新")
	s, fr := newDiscoveryScorer(t, cfg, nodes, inPool, subs, `US`)
	s.poolSet = map[string]bool{
		"ash/US-01": true, "ash/US-02": true, "ash/US-03": true, "ash/US-04": true,
	}
	ctx := context.Background()
	s.score(ctx)
	fr.waitRenders(t, 1)
	_, _, after1 := fr.snapshot()
	// /proxies still reports the old us-pool (mihomo would have been reloaded
	// in production; the mock does not update). The candidate set is therefore
	// identical next round, so nothing new should be rendered.
	s.score(ctx)
	time.Sleep(150 * time.Millisecond)
	_, _, after2 := fr.snapshot()
	if after2 > after1 {
		t.Errorf("render storm: candidate set unchanged but rendered again (%d → %d)",
			after1, after2)
	}
}

// TestDiscovery_ColdStartPickIsDeterministic is BLOCKER 2, part 1. Below
// min_pool the cold-start last resort admits unmeasured nodes; they all have
// +Inf EWMA, so the comparator never reports "less".
//
// Empirically Go's pdqsort leaves an all-equal slice untouched, so the result
// is whatever order built `cold` — which is name-sorted `candidates`, i.e.
// already deterministic. This test therefore does NOT fail if the explicit
// tiebreak is removed; it guards against a future change to sort choice or to
// how `cold` is built silently making admission order-dependent. Keeping it is
// cheap; treating it as proof that the tiebreak is load-bearing would be wrong.
func TestDiscovery_ColdStartPickIsDeterministic(t *testing.T) {
	cfg := eligibilityCfg() // min_pool_size = 3
	nodes := map[string]proxyNode{
		"sub/good": {alive: true, history: goodHistory(100)},
	}
	subs := []string{"sub/good"}
	// 8 unmeasured candidates, all Qualified via benefit-of-doubt, all +Inf EWMA.
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		tag := "sub/US-" + n
		nodes[tag] = proxyNode{alive: true, history: nil}
		subs = append(subs, tag)
	}
	pick := func() []string {
		s, _ := newDiscoveryScorer(t, cfg, nodes, []string{"sub/good"}, subs, ``)
		s.score(context.Background())
		return setToSortedSlice(inPoolNames(s.GetSnapshot()))
	}
	first := pick()
	if len(first) < 3 {
		t.Fatalf("floor not met: pool=%v", first)
	}
	for i := 0; i < 4; i++ {
		if got := pick(); strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("cold-start pick not deterministic:\n  run0=%v\n  run%d=%v", first, i+1, got)
		}
	}
}

// TestDiscovery_ColdStartPickReleasedOnceMeasured is BLOCKER 2, part 2 — the
// consequence that actually matters. The cold-start path admits unmeasured
// nodes by an arbitrary-but-stable order. If that pick then measures BADLY
// while its siblings measure well, incumbency must not pin it forever: the
// measured floor-fill sorts (dead, incumbent, ewma) with incumbency ahead of
// EWMA, so a bad early pick could keep carrying traffic while better nodes wait.
//
// Guard: once real measurements exist and enough good nodes are available to
// meet the floor on merit, the badly-measuring cold-start pick must be out of
// the routing set. This is what makes discovery safe to ship at pool<floor.
func TestDiscovery_ColdStartPickReleasedOnceMeasured(t *testing.T) {
	cfg := eligibilityCfg() // min_pool_size = 3
	cfg.HotReloadMinInterval = 0
	// Round 1: only one measured node, three unmeasured → cold start fills.
	nodes := map[string]proxyNode{
		"sub/US-good":  {alive: true, history: goodHistory(100)},
		"sub/US-bad":   {alive: true, history: nil},
		"sub/US-alt-1": {alive: true, history: nil},
		"sub/US-alt-2": {alive: true, history: nil},
	}
	subs := []string{"sub/US-good", "sub/US-bad", "sub/US-alt-1", "sub/US-alt-2"}
	const probeURL = "http://probe.test/generate_204"
	apiAddr, mutate := mockClashAPIPartialPool(t, probeURL, nodes,
		[]string{"sub/US-good"})
	fr := &fakeRenderer{path: t.TempDir() + "/config.yaml"}
	obs := make([]subscribe.Outbound, 0, len(subs))
	for _, tag := range subs {
		obs = append(obs, outbound(tag))
	}
	s := New(cfg, nil, apiAddr, "", probeURL, `US`, fr,
		func() []subscribe.Outbound { return obs })
	t.Cleanup(func() { time.Sleep(50 * time.Millisecond) })

	ctx := context.Background()
	s.score(ctx)
	if got := inPoolNames(s.GetSnapshot()); len(got) < 3 {
		t.Fatalf("cold start did not meet floor: %v", got)
	}

	// Measurements arrive: the cold-start pick is terrible, siblings are good.
	mutate("sub/US-bad", goodHistory(5000))   // p95 5000 > max_rtt_p95_ms 800
	mutate("sub/US-alt-1", goodHistory(150))
	mutate("sub/US-alt-2", goodHistory(160))
	// Two rounds: one to record measurements, one to act on them.
	s.score(ctx)
	s.score(ctx)

	pool := inPoolNames(s.GetSnapshot())
	if len(pool) < 3 {
		t.Errorf("floor breached after measurement: %v", pool)
	}
	if pool["sub/US-bad"] {
		t.Errorf("badly-measuring cold-start pick still routing while good "+
			"alternatives exist (incumbency pinned it); pool=%v", pool)
	}
}
