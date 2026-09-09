// Package nodescorer implements dynamic pool management for engine=mihomo.
//
// The scorer runs a goroutine that periodically:
//  1. Reads every node's probe history from mihomo's /proxies endpoint and
//     computes RTT statistics (p50, p95, jitter, fail_rate).
//  2. Passively reads /connections to derive per-node real-traffic throughput
//     (upload+download bytes/sec from active connections) — no extra bandwidth
//     consumed.
//  3. Compares each node against the configured NodeQualify thresholds.
//  4. If the qualified set has changed, re-renders mihomo's config.yaml with
//     the updated us-pool member list and calls PUT /configs (mihomo hot-reload).
//     Existing connections drain naturally; new connections use the new config.
//
// The scorer also exposes NodeHealth state for /api/nodes/health.
package nodescorer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
	"github.com/leap-gateway/leap-gateway/internal/subscribe"
)

// NodeHealth holds the latest scored state of one proxy node.
type NodeHealth struct {
	Name string `json:"name"`
	Sub  string `json:"sub"` // subscription prefix before first "/"

	// RTT stats from mihomo probe history (most recent MinProbes..8 entries).
	Alive      bool    `json:"alive"`
	LastRTTMs  int     `json:"rtt_last_ms"`
	RTTP50Ms   int     `json:"rtt_p50_ms"`
	RTTP95Ms   int     `json:"rtt_p95_ms"`
	JitterMs   int     `json:"jitter_ms"`   // p95 - p50
	FailRate   float64 `json:"fail_rate"`   // fraction of probes with delay=0
	ProbeCount int     `json:"probe_count"` // history entries available

	// Passive throughput from /connections (bytes/sec, 0 when no active traffic).
	ThroughputBps float64 `json:"throughput_bps"`

	// Pool membership state.
	Qualified bool   `json:"qualified"`
	InPool    bool   `json:"in_pool"`
	Strikes   int    `json:"strikes"`          // consecutive failing rounds
	OkRounds  int    `json:"ok_rounds"`        // consecutive passing rounds
	Reason    string `json:"reason,omitempty"` // why not qualified (if !Qualified)

	// Probes holds the latest site-specific probe result per probe name
	// (key = probe Name, e.g. "chatgpt.com"). Empty when no probes are
	// configured or none have run yet. Gates named-pool membership.
	Probes map[string]ProbeResult `json:"probes,omitempty"`

	// Passive holds connection-lifecycle stats derived from mihomo's
	// /connections (localhost, no airport traffic). nil when the passive
	// loop hasn't produced data yet.
	Passive *PassiveStats `json:"passive,omitempty"`
}

// PassiveStats summarizes a node's TCP connection behavior over a rolling
// window, derived purely from snapshotting mihomo's /connections. A node
// that accepts connections which immediately close having moved near-zero
// bytes is failing in a way RTT probes don't catch (upstream RST, captive
// redirect, dead egress) — this surfaces that.
type PassiveStats struct {
	ActiveConns     int     `json:"active_conns"`  // current live conns through this node
	ClosedWindow    int     `json:"closed_window"` // conns that closed during the window
	FailedWindow    int     `json:"failed_window"` // of those, ones that moved < minHandshakeBytes
	FailRate        float64 `json:"fail_rate"`     // failed/closed over the window (0 when no closes)
	SampleWindowSec int     `json:"sample_window_sec"`
}

// ProbeResult is one site-specific reachability measurement for one node.
type ProbeResult struct {
	OK            bool      `json:"ok"`           // status in range AND not a CF challenge
	StatusCode    int       `json:"status_code"`  // 0 = no response
	CFMitigated   bool      `json:"cf_mitigated"` // cf-mitigated: challenge header seen
	LatencyMs     int       `json:"latency_ms"`   // real round-trip; on failure = time waited before giving up
	LastCheckedAt time.Time `json:"last_checked_at"`
	LastError     string    `json:"last_error,omitempty"`
}

// Snapshot is the complete scorer state at one instant.
type Snapshot struct {
	Qualified      int          `json:"qualified"`
	Total          int          `json:"total"`
	LastPoolUpdate time.Time    `json:"last_pool_update"`
	LastScoredAt   time.Time    `json:"last_scored_at"`
	Thresholds     interface{}  `json:"thresholds"`
	Nodes          []NodeHealth `json:"nodes"`
	// PoolSizing surfaces the K-gating computation. KTarget=0 means
	// K-gating is disabled (legacy mode). KCurrent is the actual rendered
	// us-pool size (may differ from KTarget during hysteresis transitions).
	// SupplyLimited=true when |Tier1| was the binding ceiling — operator
	// should consider raising the acceptance bar or adding subscriptions.
	PoolSizing PoolSizingSnapshot `json:"pool_sizing"`
	// RoutingPool is the top-K subset actually rendered into mihomo's
	// us-pool (routingMembers). Empty when K-gating is disabled (full
	// qualified set is routed) or before the first scoring round.
	RoutingPool []string `json:"routing_pool"`
	// PoolMode mirrors node_qualify.pool_mode ("auto" | "manual").
	PoolMode string `json:"pool_mode"`
}

// PoolSizingSnapshot is the runtime view of the K-gating decision.
type PoolSizingSnapshot struct {
	KTarget  int `json:"k_target"`
	KCurrent int `json:"k_current"`
	TActive  int `json:"t_active"`
	// Tier1Count = len(qualifiedNodes) = nodes passing the LIVENESS gate
	// (scoreNode's Qualified bit, which includes the no-history "benefit of
	// doubt"). It does NOT apply the admission quality gate, so it is an
	// upper bound on admissible nodes, not a count of them — on 92 it read
	// 12 while 7 were in-pool and 5 were refused on fail_rate. The name is
	// historical ("tier 1" predates the admission gate); under
	// sizing_mode=eligibility it is display-only and gates nothing (kTarget
	// is overwritten with len(newPoolSet) right after computeK).
	Tier1Count    int  `json:"tier1_count"`
	SupplyLimited bool `json:"supply_limited"`
}

// EmergencyEvent records one auto-eviction or auto-promotion triggered by
// the emergency_promote_chain machinery in manual mode. Surfaced via
// /api/pool/state so the operator sees exactly what the system did and
// why, in lieu of a Push notification (out of scope for this iteration).
type EmergencyEvent struct {
	At     time.Time `json:"at"`
	Type   string    `json:"type"` // "evict" | "promote" | "exhausted"
	Node   string    `json:"node,omitempty"`
	Reason string    `json:"reason"`
}

const (
	// hardFailThreshold is the per-round fail_rate at which a node is
	// considered "hard-failing this round" — high enough to be near-100%
	// without requiring a perfectly clean window (probe noise can leave a
	// truly-dead node at 0.95 instead of 1.0). Below this it's "degraded
	// not dead" — graceful, doesn't trigger emergency.
	hardFailThreshold = 0.9

	// emergencyEvictAfter is the sustained hard-fail duration that arms
	// the auto-evict path. 30 minutes balances "user has clearly noticed
	// a dead node" against "transient outage that will self-heal" —
	// per the design discussion, IP rotation is a detection cost, so
	// only pay it once degradation is clearly persistent.
	emergencyEvictAfter = 30 * time.Minute

	// emergencyEventLogMax caps the in-memory event log size; older
	// events drop off so /api/pool/state stays small.
	emergencyEventLogMax = 50
)

// Renderer is the subset of the mihomo renderer used by NodeScorer to
// rebuild config.yaml when the pool membership changes.
type Renderer interface {
	// RenderWithPools is the production render entry. usPool = the full
	// probing set (all currently-qualified nodes; mihomo url-test probes
	// these to keep ranking signal fresh). routingMembers ⊆ usPool is the
	// subset that actually carries traffic via per-terminal HRW (top-K
	// when K-gating active; nil falls back to usPool — legacy behavior).
	// poolMembers gates named pools (us-pool ∩ probe-passing).
	RenderWithPools(outbounds []subscribe.Outbound, usPool, routingMembers []string, poolMembers map[string][]string) ([]byte, error)
	// Path returns the on-disk path of the config file.
	Path() string
}

// Scorer is the dynamic pool manager.
type Scorer struct {
	cfg         config.NodeQualifyConfig
	pools       []config.PoolConfig // named pools gated by probe results
	nodePattern string              // regexp filter matching sing-box renderer's NodePattern
	apiAddr     string              // mihomo clash-api host:port
	apiSecret   string
	probeURL    string // same URL mihomo uses for url-test
	renderer    Renderer
	subscribe   func() []subscribe.Outbound // live outbounds from subscribe.Manager

	mu      sync.RWMutex
	state   map[string]*nodeState // keyed by node tag
	snap    Snapshot
	poolSet map[string]bool // current us-pool qualified set in config

	// lastUsPoolRendered is the us-pool (PROBING) set most recently asked for
	// in a render. poolSet above is the ROUTING set; the two differ, and only
	// poolSet was ever compared to decide whether to hot-reload. That made
	// candidate discovery useless on its own: a newly-discovered node changes
	// the probing set but not necessarily the routing set (it cannot enter
	// routing until it has measurements), so no render fired, so it never
	// entered mihomo's us-pool, so mihomo never url-tested it, so it never got
	// measurements — refused forever while looking healthy.
	//
	// Compared against our own last INTENT rather than against mihomo's
	// current us-pool: usPoolMembers intersects with nodeTags(AllOutbounds),
	// so a candidate present in /proxies but absent from AllOutbounds would
	// never appear in the rendered group, and a "mihomo differs from desired"
	// trigger would then fire a reload every single round. Comparing intent is
	// idempotent by construction. HotReloadMinInterval still throttles.
	//
	// Not persisted: on restart it starts nil, so the first round renders once
	// and re-establishes it. That is correct — the process may have missed
	// subscription changes while down.
	lastUsPoolRendered map[string]bool
	// poolMembers is the last-rendered named-pool membership (pool name →
	// member tags), used to detect when a probe change requires a re-render
	// even though us-pool itself is unchanged.
	poolMembers map[string][]string
	// probeResults[nodeTag][probeName] = latest result. Written by the
	// probe loop, read by score() to gate pool membership.
	probeResults map[string]map[string]ProbeResult
	probeLast    map[string]map[string]time.Time // last run time per (node,probe)
	// probeOKHistory[nodeTag][probeName] = sliding window of last K
	// `res.OK` values, oldest-first. Read by nodePassesProbes —
	// hysteresis prevents a single noisy CF response from flipping a
	// node out of openai-pool (and triggering a hot-reload). A node
	// is "passing" if ANY entry in the last K is true; it falls out
	// only after K consecutive failures.
	probeOKHistory map[string]map[string][]bool

	httpc      *http.Client // clash-api client
	probeHTTPc *http.Client // dials through the leap-probe listener

	// throughput tracking: previous /connections snapshot, keyed by
	// CONNECTION ID (not node). Keyed by node until 2026-08-12, which made
	// the delta wrong in both directions — see updateThroughput.
	//
	// Deliberately separate state from connSeen below, even though both
	// track live connections: passivePoll runs on its own 10s ticker while
	// updateThroughput runs once per scoring round, so sharing one map
	// would make each overwrite the other's baseline and corrupt both
	// time deltas.
	connPrev  map[string]connBytes
	connPrevT time.Time

	// lastHotReload is the wall-clock time of the most recent successful
	// (or attempted) clash-api PUT /configs. score() consults this to
	// rate-limit reloads via cfg.HotReloadMinInterval; transient pool
	// flaps inside the window get deferred to the next scoring round
	// rather than firing a fresh reload each time.
	lastHotReload time.Time

	// Emergency state (manual mode only). The "effective pool" is what
	// mihomo currently routes through; it equals cfg.PoolMembers minus
	// auto-evicted entries plus auto-promoted entries from
	// cfg.EmergencyPromoteChain. yamlBaseline is the snapshot of
	// cfg.PoolMembers at the time effective was last in sync — used at
	// startup to detect "ops edited yaml" vs "we drifted via emergency"
	// (yaml edit resets effective to the new baseline). emergencyEvents
	// is a recent log surfaced via /api/pool/state.
	effectivePool   []string
	yamlBaseline    []string
	emergencyEvents []EmergencyEvent

	// transitions is the audit log of pool composition changes (auto K-
	// gating swaps, manual emergency events, operator API actions). Capped
	// at transitionLogMax; persisted in state file v3. Surfaced via
	// /api/pool/transitions; consumed by /api/pool/rollback.
	transitions []PoolTransition

	// rollbackQuarantine is node → until-time. K-gating auto mode refuses
	// to re-add a quarantined node before until passes — keeps a rollback
	// from being immediately undone by the next scoring round. Cleaned up
	// lazily on read (inQuarantine deletes expired entries).
	rollbackQuarantine map[string]time.Time

	// ewma is the per-node smoothed health signal — short window (4h)
	// drives evict, long window (24h) drives promote. Persisted in the
	// state file v4 schema; nil-safe (lazy-initialized in updateNodeEWMA).
	// See ewma.go for the math + half-life rationale.
	ewma map[string]*nodeEWMA

	// passive connection-lifecycle tracking (1.6). connSeen maps live
	// connID → its node + last-seen byte total; closeEvents is a rolling
	// per-node log of recently-closed connections used to compute fail
	// rate. Both guarded by s.mu.
	connSeen    map[string]connInfo
	closeEvents map[string][]closeEvent
	activeConns map[string]int // node → current live conn count

	// eventHook fires when the scorer records an EmergencyEvent — the
	// notify subsystem registers here to forward the event to Lark / the
	// local JSONL log. nil-safe (called only when set). Wired via
	// SetEventHook from main.go after construction so the scorer package
	// stays free of notify imports.
	eventHook func(EmergencyEvent)

	// transitionHook fires on every pool composition change recorded
	// via recordTransitionLocked (auto K-gating swaps, manual emergency
	// transitions, operator rollbacks). main.go wires this to the same
	// notify subsystem with a different formatter, so pool drifts
	// surface in Lark — without this, K-gating swaps were silent.
	transitionHook func(PoolTransition)
}

// connInfo is the last-observed state of one live connection.
type connInfo struct {
	node  string
	bytes int64 // upload+download at last poll
}

// closeEvent records a connection that disappeared between polls.
type closeEvent struct {
	at     time.Time
	failed bool // moved < minHandshakeBytes before closing
}

type nodeState struct {
	health  NodeHealth
	strikes int
	okRuns  int
	// admitRuns: consecutive scoring rounds this NON-MEMBER has passed the
	// admission quality gate (admissionRefusal == ""). Reset to 0 while the
	// node is in the pool, so a node that exits must re-earn entry from
	// scratch. Drives the ReadmitStrikes entry gate under sizing_mode=
	// eligibility — see computeEligibleSetLocked. NOT persisted: it is a
	// short-horizon anti-flap counter, and starting at 0 after a restart is
	// the conservative direction (bootstrap is exempt anyway).
	admitRuns int
	// hardFailStart is the wall-clock time the scorer first observed
	// fail_rate >= hardFailThreshold for this node in an unbroken streak.
	// Zero when the node is healthy. Cleared (zero again) the moment a
	// scoring round shows fail_rate < hardFailThreshold. emergency-eviction
	// fires when a pool member has been hard-failing for >=
	// EmergencyEvictAfter wall-clock time.
	hardFailStart time.Time
	// deadRounds: consecutive scoring rounds observed alive=false (mihomo
	// cannot connect). Incremented each alive=false round, reset to 0 when
	// alive. Drives dead-eviction of soft-dead incumbents at
	// cfg.DeadEvictRounds. NOT persisted (not in persistedState) — resets
	// to 0 on restart, so a restored incumbent gets a fresh grace window
	// rather than being instant-evicted on one post-restart dead round.
	deadRounds int
	// lastRefusal is the most recent admission-gate refusal reason for this
	// node, used to log only on TRANSITION. Without it a node held out by the
	// gate logs an identical line every scoring round (at ~24 candidates that
	// is thousands of lines/day saying nothing new). Not persisted — a restart
	// legitimately re-announces why a node is being kept out.
	lastRefusal string
}

// connBytes is one connection's last-observed cumulative counters, plus
// the node it egresses through (needed to attribute its delta after the
// connection is gone from the live set).
type connBytes struct {
	node     string
	upload   int64
	download int64
}

// New creates a Scorer. probeURL should match mihomo's url-test URL (same
// probe target = same measurement conditions). pools are the named pools
// whose membership the scorer gates on site-specific probe results.
func New(
	cfg config.NodeQualifyConfig,
	pools []config.PoolConfig,
	apiAddr, apiSecret, probeURL, nodePattern string,
	r Renderer,
	allOutbounds func() []subscribe.Outbound,
) *Scorer {
	// probeHTTPc dials through the leap-probe listener (127.0.0.1:probePort)
	// so each request egresses via whatever node probe-out currently points
	// at. Short timeout — a probe that hangs is itself a failure signal.
	probeProxy, _ := url.Parse("http://127.0.0.1:11081")
	return &Scorer{
		cfg:                cfg,
		pools:              append([]config.PoolConfig(nil), pools...),
		nodePattern:        nodePattern,
		apiAddr:            apiAddr,
		apiSecret:          apiSecret,
		probeURL:           probeURL,
		renderer:           r,
		subscribe:          allOutbounds,
		state:              map[string]*nodeState{},
		poolSet:            map[string]bool{},
		poolMembers:        map[string][]string{},
		probeResults:       map[string]map[string]ProbeResult{},
		probeLast:          map[string]map[string]time.Time{},
		probeOKHistory:     map[string]map[string][]bool{},
		connPrev:           map[string]connBytes{},
		connSeen:           map[string]connInfo{},
		closeEvents:        map[string][]closeEvent{},
		activeConns:        map[string]int{},
		rollbackQuarantine: map[string]time.Time{},
		ewma:               map[string]*nodeEWMA{},
		httpc:              &http.Client{Timeout: 5 * time.Second},
		probeHTTPc: &http.Client{
			Timeout:   12 * time.Second,
			Transport: &http.Transport{Proxy: http.ProxyURL(probeProxy)},
		},
	}
}

// probesEnabled reports whether any configured pool gates on probes (i.e.
// the renderer emitted probe-out + the leap-probe listener, so the probe
// loop has something to drive).
func (s *Scorer) probesEnabled() bool {
	if len(s.cfg.Probes) == 0 {
		return false
	}
	for _, p := range s.pools {
		if len(p.RequiresPassing) > 0 {
			return true
		}
	}
	return false
}

// Run blocks until ctx is cancelled. Each ScoringInterval it scores all
// nodes and hot-reloads mihomo when the pool changes.
func (s *Scorer) Run(ctx context.Context) {
	s.loadState()
	slog.Info("nodescorer: started",
		"interval", s.cfg.ScoringInterval,
		"max_rtt_p50", s.cfg.MaxRTTP50Ms,
		"max_rtt_p95", s.cfg.MaxRTTP95Ms,
		"max_jitter", s.cfg.MaxJitterMs,
		"max_fail_rate", s.cfg.MaxFailRate,
		"evict_strikes", s.cfg.EvictStrikes,
		"readmit_strikes", s.cfg.ReadmitStrikes,
		"hot_reload_min_interval", s.cfg.HotReloadMinInterval,
	)
	// Initial score after a short warm-up so probe history is populated.
	warmup := time.NewTimer(10 * time.Second)
	select {
	case <-ctx.Done():
		return
	case <-warmup.C:
	}
	s.score(ctx)

	// Probe loop runs independently on its own cadence — flipping the
	// probe-out selector is serialized within that single goroutine, so it
	// never races itself. Only started when a pool gates on probes.
	if s.probesEnabled() {
		go s.runProbeLoop(ctx)
	}

	// Passive connection-lifecycle stats: poll /connections every ~10s
	// (localhost, no airport traffic) to derive per-node fail rate.
	go s.runPassiveLoop(ctx)

	tick := time.NewTicker(s.cfg.ScoringInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.score(ctx)
		}
	}
}

// GetSnapshot returns a copy of the latest scored state.
func (s *Scorer) GetSnapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := s.snap
	nodes := make([]NodeHealth, len(snap.Nodes))
	copy(nodes, snap.Nodes)
	snap.Nodes = nodes
	return snap
}

func (s *Scorer) score(ctx context.Context) {
	// 1. Fetch /proxies from mihomo.
	proxies, err := s.fetchProxies(ctx)
	if err != nil {
		slog.Warn("nodescorer: /proxies fetch failed", "err", err)
		return
	}

	// 2. Fetch /connections for passive throughput.
	throughput := s.updateThroughput(ctx, proxies)

	// 3. Build candidate set = union of THREE sources:
	//      (a) current us-pool members,
	//      (b) nodes previously seen in state (keeps evicted nodes under
	//          observation so they can recover and be readmitted),
	//      (c) subscription outbounds matching NodePattern (DISCOVERY).
	//
	// (c) is what makes a newly-added subscription work. Without it (a) and (b)
	// form a closed loop: the candidate set comes from us-pool, and us-pool's
	// membership is written by the renderer from the scorer's own
	// qualifiedOverride, which comes from the candidate set. A node the scorer
	// has never seen has no way in — it must already be in us-pool to be
	// scored, and must be scored to enter us-pool. Measured on 92: 11 US nodes
	// from two ACTIVE subscriptions were never scored once across 8 consecutive
	// rounds, and a process restart did NOT rescue them (main.go skips the
	// bootstrap render when config.yaml exists, so mihomo starts with the
	// previously-rendered us-pool and the pattern fallback in usPoolMembers
	// never fires). See docs/ops-candidate-discovery-loop.md.
	//
	// HISTORY — the discovery this restores was removed by b1ff1df on a stated
	// rationale that is FALSE: it claimed Go RE2's \b "produced no matches" for
	// ctc-02/US-C30-* names. Verified against the exact production pattern, all
	// such names match (\b is an ASCII word boundary; 🇺🇸/美国/·/- are all
	// non-word chars). That commit's real fix was the state union in (b), which
	// solved a genuine bug (us-pool shrinking to the qualified subset starved
	// the candidate source). Dropping the regexp was collateral damage from a
	// wrong theory, and it created the loop.
	//
	// Union, never replace: a superset cannot regress either of b1ff1df's bugs
	// (evicted nodes keep watchlist status via (b); the empty-candidate early
	// return below is structurally unreachable if the set only grows).
	//
	// Source is AllOutbounds, not the /proxies keys: /proxies also contains
	// groups and built-ins (us-pool, out, pin, probe-out, fb-<ip>, DIRECT...),
	// and an empty NodePattern means "everything", which would then score the
	// groups themselves as nodes. AllOutbounds is subscription outbounds only,
	// and matches the renderer's own source of truth (nodeTags in groups.go),
	// which is what keeps the reload trigger below idempotent.
	//
	// Called before taking s.mu: s.subscribe takes the manager's own RWMutex
	// and there is no reason to nest the two.
	discovered := filterCandidates(s.subscribe(), s.nodePattern, proxies)

	// us-pool absence is no longer fatal. groups.go deliberately omits the
	// group when it would be empty (all subscriptions failed / bootstrap), and
	// returning here parked the scorer until an unrelated refresh re-rendered
	// it — a self-perpetuating dead state adjacent to the loop above. With
	// discovery available, source (a) is optional.
	var members []interface{}
	if usPool := proxies["us-pool"]; usPool != nil {
		members, _ = usPool["all"].([]interface{})
	} else if len(discovered) == 0 {
		slog.Warn("nodescorer: us-pool not found in /proxies and no nodes matched node_pattern")
		return
	} else {
		slog.Warn("nodescorer: us-pool not found in /proxies — proceeding on pattern discovery",
			"discovered", len(discovered))
	}
	candidateSet := make(map[string]bool, len(members)+len(s.state)+len(discovered))
	for _, raw := range members {
		if tag, ok := raw.(string); ok && tag != "" {
			candidateSet[tag] = true
		}
	}
	for tag := range s.state {
		// Re-include previously scored nodes only if they still exist in /proxies
		if _, ok := proxies[tag]; ok {
			candidateSet[tag] = true
		}
	}
	newlyDiscovered := make([]string, 0, len(discovered))
	for _, tag := range discovered {
		if !candidateSet[tag] {
			newlyDiscovered = append(newlyDiscovered, tag)
		}
		candidateSet[tag] = true
	}
	if len(newlyDiscovered) > 0 {
		// Logged once per node, on the round it first becomes a candidate.
		sort.Strings(newlyDiscovered)
		slog.Info("nodescorer: discovered new candidates via node_pattern",
			"nodes", newlyDiscovered, "count", len(newlyDiscovered))
	}
	candidates := make([]string, 0, len(candidateSet))
	for tag := range candidateSet {
		candidates = append(candidates, tag)
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		slog.Warn("nodescorer: no candidates (us-pool empty, no prior state, no pattern match)")
		return
	}

	// 4. Score each member.
	s.mu.Lock()
	defer s.mu.Unlock()

	newPoolSet := map[string]bool{}
	nodes := make([]NodeHealth, 0, len(candidates))

	for _, tag := range candidates {
		pData, ok := proxies[tag]
		if !ok {
			continue
		}
		h := s.scoreNode(tag, pData, throughput[tag])
		// Retrieve or create state.
		st, exists := s.state[tag]
		if !exists {
			st = &nodeState{}
			s.state[tag] = st
		}
		st.health = h

		// Attach the latest probe results (read-only copy) for this node.
		if pr := s.probeResults[tag]; len(pr) > 0 {
			cp := make(map[string]ProbeResult, len(pr))
			for k, v := range pr {
				cp[k] = v
			}
			h.Probes = cp
		}
		// Attach passive connection-lifecycle stats (computed under the
		// same lock we already hold in score()).
		h.Passive = s.passiveStatsLocked(tag)
		st.health = h
		nodes = append(nodes, h)
	}

	// Update per-node EWMA from this round's composite scores. Drives
	// the auto-mode evict/promote decisions in the K-gating branch
	// below — short window (4h) for evict, long window (24h) for
	// promote. See ewma.go for the math + half-life rationale.
	//
	// CRITICAL: skip no-measurement rounds. compositeScore returns the
	// 1e9 sentinel for a node with ProbeCount==0 (so it ranks last in a
	// single round's ordering). Feeding that sentinel into the EWMA
	// poisons it: the smoothed value seeds at 1e9 and decays at the 24h
	// half-life, swamping the real scores (~200–2800) by ~6 orders of
	// magnitude for days. Every node's long EWMA then converges to "the
	// decayed sentinel", so the promote ranking + SwapThresholdScore gate
	// (default 100) compare differences of ~millions against a margin of
	// 100 — i.e. they become noise, and the pool flaps. Caught on 92
	// (2026-06-12): the 8th slot ping-ponged between two nodes ~18×/24h
	// because the anti-flap gate was operating on garbage EWMA values.
	// Only real measurements feed the EWMA; no-data nodes keep "no signal"
	// (nodeLongEWMA → +Inf → ranks last), which is the correct treatment.
	ewmaNow := time.Now()
	for i := range nodes {
		if nodes[i].ProbeCount == 0 {
			continue
		}
		s.updateNodeEWMA(nodes[i].Name, compositeScore(nodes[i]), ewmaNow)
	}

	// us-pool membership policy: K-gated ranking is always on.
	//
	// K-gated mode:
	//   1. Compute K from the operator's PoolSizing config + current
	//      qualified count (computeK in helpers.go).
	//   2. Rank Qualified candidates ascending by long EWMA (lower=better).
	//   3. Top-K is the "preferred set" for this round.
	//   4. Asymmetric hysteresis:
	//      - Enter pool: requires ReadmitStrikes consecutive rounds in
	//        preferred set (default 1). Bootstrap fills greedily.
	//      - Exit pool: immediate when pool > MinPoolSize. When pool would
	//        drop below MinPoolSize, retain the best available nodes
	//        (including cold-start/trial nodes) as fallback.
	//   5. HotReloadMinInterval still throttles reload frequency.
	//
	// Why this preserves anti-detection: per-terminal HRW (perterminal.go)
	// gives each terminal one stable egress; K-gating keeps the pool
	// small + uniformly high-quality so HRW never lands a terminal on a
	// degraded node. Pool changes are slow (readmit throttle), so each
	// terminal's egress IP is stable for hours-to-days.
	qualifiedNodes := make([]*NodeHealth, 0, len(nodes))
	for i := range nodes {
		if nodes[i].Qualified {
			qualifiedNodes = append(qualifiedNodes, &nodes[i])
		}
	}
	kTarget, supplyLimited := computeK(s.cfg.PoolSizing, len(qualifiedNodes))
	minPool := s.cfg.PoolSizing.MinPoolSize
	if minPool <= 0 {
		minPool = 3
	}
	now := time.Now()

	switch s.cfg.PoolMode {
	case "manual":
		// Manual mode: pool composition is operator-owned via
		// cfg.PoolMembers (the "yaml baseline"). Scorer is observation-
		// driven for ranking, but does run ONE auto path —
		// emergency_promote_chain — to keep the data plane alive when
		// a member dies and the operator is offline.
		//
		// Two-state model:
		//   yamlBaseline    = sorted snapshot of cfg.PoolMembers
		//   effectivePool   = baseline ± emergency mutations (persisted)
		//
		// On yaml edit: reconcileBaseline detects the change and resets
		// effective to the new baseline (yaml change wins, drops emergency
		// state).
		//
		// On hard-failure (fail_rate >= 0.9 sustained 30min): evict that
		// member, promote next chain entry, hot-reload.
		s.reconcileBaseline(s.cfg.PoolMembers)

		// Update per-node hard-fail timers + identify ripe evictions.
		ripe := s.updateHardFailTimers(nodes, time.Now())

		// Apply emergency mutations to s.effectivePool (mutates state).
		emergencyMutated := s.applyEmergencyEvictions(ripe, candidateSet, time.Now())

		// Build newPoolSet from current effective pool. Intersect with
		// present candidates to match perterm's render-time guard.
		manualSet := make(map[string]bool, len(s.effectivePool))
		for _, m := range s.effectivePool {
			if candidateSet[m] {
				manualSet[m] = true
			}
		}
		// Surface gaps loudly: effective pool references nodes not in
		// candidates → subscription rename or stale state.
		if len(manualSet) < len(s.effectivePool) {
			missing := make([]string, 0, len(s.effectivePool)-len(manualSet))
			for _, m := range s.effectivePool {
				if !manualSet[m] {
					missing = append(missing, m)
				}
			}
			slog.Warn("nodescorer: effective pool members missing from candidates",
				"requested", len(s.effectivePool),
				"resolved", len(manualSet),
				"missing", missing)
		}
		for i := range nodes {
			st := s.state[nodes[i].Name]
			if manualSet[nodes[i].Name] {
				newPoolSet[nodes[i].Name] = true
				nodes[i].InPool = true
			}
			// Maintain strikes/okRuns for diagnostics.
			if nodes[i].Qualified {
				st.strikes = 0
				st.okRuns++
			} else {
				st.okRuns = 0
				st.strikes++
			}
			nodes[i].Strikes = st.strikes
			nodes[i].OkRounds = st.okRuns
			st.health = nodes[i]
		}
		// Surface yaml baseline as KTarget for /api/status visibility.
		kTarget = len(s.cfg.PoolMembers)
		supplyLimited = len(manualSet) < len(s.effectivePool)
		_ = emergencyMutated // surface via /api/pool/state; reload comes from poolChanged below

	case "auto", "":
		if s.cfg.SizingMode == "eligibility" {
			// Eligibility-set selection (docs/design-eligibility-set-selection.md).
			// No fixed-K target, no ranking cut, no fast-evict, no swap
			// threshold, no fill post-pass. The routing set is simply every
			// node that passes the existing liveness gate (Qualified) and the
			// two admission filters, fed whole to per-terminal HRW. K survives
			// only as a redundancy floor (minPool) with a stable fill. This
			// eliminates the marginal-slot ping-pong by construction: there is
			// no slot to contest.
			s.eligibilityPoolLocked(nodes, newPoolSet, minPool, now)
			// kTarget/supplyLimited are display-only under eligibility mode;
			// recompute supplyLimited against the floor, and surface the
			// eligible count as kTarget so /api/status reads sensibly.
			kTarget = len(newPoolSet)
			supplyLimited = len(newPoolSet) < minPool
		} else {
			// K-gated mode (legacy). Two-window EWMA decisions:
			//   - Sort qualified by LONG EWMA (24h half-life) → drives the
			//     promote ranking. Conservative: a recently-recovered node
			//     stays out until the long window forgets the bad period.
			//   - Trial filter: nodes with <24h history can't be promoted from
			//     outside the pool — they need to prove stability first.
			//     Already-in-pool trial nodes stay (rotating them out for
			//     being new defeats the bootstrap path).
			//   - Quarantine filter: nodes recently rolled-back stay out.
			//   - Absolute swap threshold: promoting a non-pool candidate
			//     over a current member requires the candidate's long
			//     EWMA to be better by >= cfg.SwapThresholdScore. Anti-flap.
			//
			// Catastrophic-fail filtering happens upstream in scoreNode
			// (fail_rate >= 0.9 sets Qualified=false → node never enters
			// qualifiedNodes). Smaller failures stay in the ranking and
			// flow naturally through compositeScore's quadratic fail term.
			sort.SliceStable(qualifiedNodes, func(i, j int) bool {
				return s.nodeLongEWMA(qualifiedNodes[i].Name) < s.nodeLongEWMA(qualifiedNodes[j].Name)
			})
			preferredSet := make(map[string]bool, kTarget)
			for i := 0; i < len(qualifiedNodes) && len(preferredSet) < kTarget; i++ {
				name := qualifiedNodes[i].Name
				if s.inQuarantine(name, now) {
					continue
				}
				// Trial nodes (no 24h history) can't be promoted from
				// outside the pool — they need to prove stability first.
				// Already-in-pool trial nodes stay (rotating them out for
				// being new defeats the bootstrap path).
				if s.inTrial(name, now) && !s.poolSet[name] {
					continue
				}
				preferredSet[name] = true
			}

			// Fast eviction on the SHORT window. This is the other half of
			// the two-window design described above, which was written but
			// never wired: nodeShortEWMA (4h half-life, docstring "for
			// evict decisions") had zero callers until 2026-08-12, so every
			// ranking AND eviction decision ran on the 24h long window.
			//
			// Why that hurt: at a 24h half-life and a 5min scoring round,
			// alpha is ~0.0024. A pool member degrading from 247ms to
			// 3000ms takes ~23 rounds (1.9h) for its long EWMA to even
			// cross the worst other pool member, so it keeps carrying
			// terminals for hours after users feel it. Measured drift on 92
			// the same day: Hutao/BGP_D had composite 252 (2nd best in
			// pool) while its long EWMA read 499 (worst in pool), and
			// US-02·AWS-SG had composite 301 with long EWMA 262 (best) —
			// the ranking was being driven by hours-old history that
			// contradicted current measurements.
			//
			// Deliberately asymmetric, and deliberately NOT a swap of the
			// sort key above:
			//   - promote still uses the LONG window. That conservatism is
			//     the point (see the comment block above): a node that just
			//     recovered should not be trusted until the long window
			//     forgets its bad period.
			//   - eviction uses the SHORT window, and only for nodes
			//     ALREADY in the pool. Nothing here can promote, so this
			//     adds no new flap source: who backfills is still decided
			//     by long-EWMA ranking plus the swap threshold.
			//
			// Trigger is relative to the pool's own short-EWMA median, not
			// an absolute ms figure, so it travels across subscription
			// tiers and does not need retuning when the whole pool is
			// slow. MinPoolSize is respected by the post-pass below.
			evicted := map[string]bool{}
			if med := s.poolShortEWMAMedian(); med > 0 {
				limit := med * evictShortEWMAFactor
				for name := range s.poolSet {
					e := s.nodeShortEWMA(name)
					if math.IsInf(e, 1) || e <= limit {
						continue
					}
					// Never evict the last usable members on this signal
					// alone: with |pool| at or below the floor, a slow node
					// still beats no node.
					if len(s.poolSet)-len(evicted) <= minPool {
						break
					}
					evicted[name] = true
					delete(preferredSet, name)
					slog.Warn("nodescorer: fast-evict on short EWMA",
						"node", name, "short_ewma", e, "pool_median", med,
						"limit", limit, "factor", evictShortEWMAFactor)
				}
			}

			// Absolute swap threshold: even when a non-pool candidate is
			// in top-K by EWMA, we only promote if its long EWMA is
			// SIGNIFICANTLY better than the worst current pool member's
			// long EWMA. Default 0 = no extra gate.
			swapThreshold := s.cfg.SwapThresholdScore
			if swapThreshold > 0 {
				worstInPoolEWMA := -math.Inf(1)
				for tag := range s.poolSet {
					if e := s.nodeLongEWMA(tag); e > worstInPoolEWMA {
						worstInPoolEWMA = e
					}
				}
				for name := range preferredSet {
					if s.poolSet[name] {
						continue
					}
					if s.nodeLongEWMA(name)+swapThreshold > worstInPoolEWMA {
						delete(preferredSet, name)
					}
				}
			}

			// First-round bootstrap: pool is empty. Fill greedily from
			// preferred so the data plane has a healthy us-pool immediately.
			bootstrap := len(s.poolSet) == 0

			for i := range nodes {
				st := s.state[nodes[i].Name]
				currentlyInPool := s.poolSet[nodes[i].Name]
				wantInPool := preferredSet[nodes[i].Name]

				if wantInPool {
					// Top-K candidate: increment okRuns, reset strikes.
					st.strikes = 0
					st.okRuns++
					if bootstrap || currentlyInPool || st.okRuns >= s.cfg.ReadmitStrikes {
						newPoolSet[nodes[i].Name] = true
						nodes[i].InPool = true
					}
				} else {
					// Not in top-K (or not qualified): exit immediately.
					// No EvictStrikes hysteresis — ranking is the sole criterion.
					// MinPoolSize safety net is applied in the post-pass below.
					st.okRuns = 0
					st.strikes++
				}
				nodes[i].Strikes = st.strikes
				nodes[i].OkRounds = st.okRuns
				st.health = nodes[i]
			}

			// Post-pass — enforce |pool| == kTarget, with MinPoolSize as hard floor.
			//
			//   under kTarget: readmit throttle blocked some preferred members;
			//     fill greedily from preferredSet first, then (if still under
			//     minPool) from all qualified nodes including trial nodes.
			//   over kTarget: shouldn't happen with immediate eviction, but
			//     guard anyway — drop worst non-preferred members.
			//
			// Two constraints this pass MUST respect, both added 2026-08-12:
			//
			//   1. Fast-evicted nodes stay out. Without this the short-EWMA
			//      eviction above is undone on the very next lines — the
			//      node is not in newPoolSet, so it looks like a free slot.
			//
			//   2. A composite ceiling. This pass ignores inTrial and
			//      inQuarantine by design (a slot must be filled), but it
			//      also ignored quality entirely, and `Qualified` is a
			//      liveness gate rather than a quality one: it deliberately
			//      does not test RTT/jitter (see scoreNode — thresholds at
			//      30s granularity churned the pool every cycle). Measured
			//      on 92: ash/US-04·GCP carried p95=5000ms, jitter=4587,
			//      composite=14674 with Qualified=true, against a
			//      configured MaxRTTP95Ms of 800. Ranking normally keeps
			//      such a node out of a full pool, but THIS path bypasses
			//      ranking, so it was the one way a node that bad could
			//      start carrying terminals.
			if len(newPoolSet) < kTarget {
				ceil := s.poolFillCeiling(nodes)
				for i := 0; i < len(qualifiedNodes); i++ {
					if len(newPoolSet) >= kTarget {
						break
					}
					name := qualifiedNodes[i].Name
					if newPoolSet[name] || evicted[name] {
						continue
					}
					if ceil > 0 {
						if c := compositeScore(*qualifiedNodes[i]); c > ceil {
							slog.Warn("nodescorer: fill candidate rejected by composite ceiling",
								"node", name, "composite", c, "ceiling", ceil)
							continue
						}
					}
					newPoolSet[name] = true
					for j := range nodes {
						if nodes[j].Name == name {
							nodes[j].InPool = true
							break
						}
					}
				}
			}
			// MinPoolSize safety net: if pool is still under floor, admit
			// trial and quarantined nodes (best available, sorted by EWMA).
			// Only Qualified nodes are eligible — catastrophic-fail nodes
			// (Qualified=false) must never carry traffic.
			//
			// This is the last resort, so unlike the kTarget fill above it
			// applies NO composite ceiling and does not exclude fast-evicted
			// nodes: below the floor, a slow node beats no node. Evicted
			// ones are merely sorted last, so they are taken only when
			// nothing else is left.
			if len(newPoolSet) < minPool {
				type fallback struct {
					name    string
					ewma    float64
					evicted bool
				}
				candidates := make([]fallback, 0, len(nodes))
				for i := range nodes {
					if nodes[i].Qualified && nodes[i].ProbeCount > 0 && !newPoolSet[nodes[i].Name] {
						candidates = append(candidates, fallback{
							nodes[i].Name,
							s.nodeLongEWMA(nodes[i].Name),
							evicted[nodes[i].Name],
						})
					}
				}
				sort.Slice(candidates, func(i, j int) bool {
					if candidates[i].evicted != candidates[j].evicted {
						return !candidates[i].evicted
					}
					return candidates[i].ewma < candidates[j].ewma
				})
				for _, c := range candidates {
					if len(newPoolSet) >= minPool {
						break
					}
					newPoolSet[c.name] = true
					for j := range nodes {
						if nodes[j].Name == c.name {
							nodes[j].InPool = true
							break
						}
					}
				}
			}
			if len(newPoolSet) > kTarget {
				type drop struct {
					name  string
					score float64
				}
				scoreOf := make(map[string]float64, len(nodes))
				for i := range nodes {
					scoreOf[nodes[i].Name] = compositeScore(nodes[i])
				}
				drops := make([]drop, 0, len(newPoolSet))
				for name := range newPoolSet {
					if !preferredSet[name] {
						drops = append(drops, drop{name, scoreOf[name]})
					}
				}
				sort.Slice(drops, func(i, j int) bool {
					return drops[i].score > drops[j].score
				})
				for _, d := range drops {
					if len(newPoolSet) <= kTarget {
						break
					}
					// Never drop below minPool.
					if len(newPoolSet) <= minPool {
						break
					}
					delete(newPoolSet, d.name)
					for j := range nodes {
						if nodes[j].Name == d.name {
							nodes[j].InPool = false
							break
						}
					}
				}
			}
		}
	}
	_ = supplyLimited // surfaced via /api/status, not gating logic

	// Shadow mode: while legacy is live, compute what eligibility-set
	// selection WOULD pick this round and log the diff, changing nothing.
	// Pre-cutover validation — run ≥24h, read the diffs, then flip
	// sizing_mode. Pure (computeEligibleSetLocked mutates nothing), so it
	// cannot perturb the live legacy decision above.
	if s.cfg.SizingShadow && s.cfg.SizingMode != "eligibility" && s.cfg.PoolMode != "manual" {
		shadow := s.computeEligibleSetLocked(nodes, minPool, now)
		// diffPools(a,b) → (added=b\a, removed=a\b). With a=live, b=shadow:
		// added = shadowOnly (eligibility would add), removed = liveOnly
		// (legacy keeps, eligibility would drop).
		shadowOnly, liveOnly := diffPools(setToSortedSlice(newPoolSet), setToSortedSlice(shadow))
		slog.Info("nodescorer: shadow eligibility diff",
			"live_pool", len(newPoolSet),
			"shadow_pool", len(shadow),
			"only_in_live", liveOnly,      // legacy keeps, eligibility would drop
			"only_in_shadow", shadowOnly,  // eligibility would add, legacy excludes
			"identical", len(liveOnly) == 0 && len(shadowOnly) == 0,
		)
	}

	// Sort: qualified first, then by RTT.
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Qualified != nodes[j].Qualified {
			return nodes[i].Qualified
		}
		return nodes[i].RTTP50Ms < nodes[j].RTTP50Ms
	})

	qualified := 0
	for _, n := range nodes {
		if n.InPool {
			qualified++
		}
	}

	now = time.Now().UTC()
	poolChanged := !poolSetsEqual(s.poolSet, newPoolSet)

	// The probing set changing is also a reason to re-render, independently of
	// the routing set. See lastUsPoolRendered.
	usPoolChanged := !poolSetsEqual(s.lastUsPoolRendered, candidateSet)

	// Compute named-pool memberships: us-pool ∩ {nodes passing the pool's
	// required probes}. A node with no probe result yet for a required
	// probe is treated as NOT passing (conservative — keep unverified
	// nodes out of the sensitive pool rather than risk a 403).
	newPoolMembers := s.computePoolMembers(newPoolSet)
	if !poolMembersEqual(s.poolMembers, newPoolMembers) {
		poolChanged = true
	}

	lastPoolUpdate := s.snap.LastPoolUpdate
	if poolChanged {
		lastPoolUpdate = now
	}

	// Throttle: if a pool change is detected but we just hot-reloaded,
	// defer the change to the next scoring round. This keeps a single
	// flapping node from triggering back-to-back reloads (each clash-api
	// PUT /configs?force=true rebuilds the LoadBalance hash ring and can
	// disrupt long-lived flows). poolSet/poolMembers stay pointing at
	// the runtime applied set so hysteresis (EvictStrikes/ReadmitStrikes)
	// continues to compare against what mihomo actually has.
	// usPoolChanged is throttled the same way: a probing-set-only change is
	// not urgent enough to bypass the flow-disruption guard.
	reloadNeeded := poolChanged || usPoolChanged
	poolChangePending := reloadNeeded &&
		!s.lastHotReload.IsZero() &&
		s.cfg.HotReloadMinInterval > 0 &&
		now.Sub(s.lastHotReload) < s.cfg.HotReloadMinInterval

	if poolChangePending {
		for i := range nodes {
			nodes[i].InPool = s.poolSet[nodes[i].Name]
		}
		qualified = 0
		for _, n := range nodes {
			if n.InPool {
				qualified++
			}
		}
		slog.Info("nodescorer: pool change deferred (throttle)",
			"proposed_pool", len(newPoolSet),
			"applied_pool", len(s.poolSet),
			"since_last_reload", now.Sub(s.lastHotReload).Round(time.Second),
			"min_interval", s.cfg.HotReloadMinInterval,
		)
		lastPoolUpdate = s.snap.LastPoolUpdate
	}

	// routingPool = the nodes actually rendered into per-terminal HRW:
	// top-K (newPoolSet) when K-gating active, else all qualified candidates.
	var routingPool []string
	if kTarget > 0 {
		routingPool = setToSortedSlice(newPoolSet)
	} else {
		routingPool = make([]string, 0, len(qualifiedNodes))
		for _, qn := range qualifiedNodes {
			routingPool = append(routingPool, qn.Name)
		}
		sort.Strings(routingPool)
	}
	s.snap = Snapshot{
		Qualified:      qualified,
		Total:          len(nodes),
		LastPoolUpdate: lastPoolUpdate,
		LastScoredAt:   now,
		Thresholds:     s.cfg,
		Nodes:          nodes,
		RoutingPool:    routingPool,
		PoolMode:       s.cfg.PoolMode,
		PoolSizing: PoolSizingSnapshot{
			KTarget:       kTarget,
			KCurrent:      len(newPoolSet),
			TActive:       s.cfg.PoolSizing.TActive,
			Tier1Count:    len(qualifiedNodes),
			SupplyLimited: supplyLimited,
		},
	}
	if !poolChangePending {
		// Capture pre-mutation state for transition log + diff against
		// newPoolSet. Audit log entry only when pool actually changed
		// (poolChanged includes named-pool member shifts that may not
		// affect us-pool composition — record both kinds).
		if poolChanged && s.cfg.PoolMode != "manual" {
			before := setToSortedSlice(s.poolSet)
			after := setToSortedSlice(newPoolSet)
			added, removed := diffPools(before, after)
			if len(added) > 0 || len(removed) > 0 {
				// Distinguish bootstrap (poolSet empty after a restart
				// — first scoring round always "adds" everyone) from a
				// real K-gating drift. Bootstrap is recorded for audit
				// transparency but main.go's notify formatter skips it
				// to avoid Lark spam on every redeploy.
				txType := "auto_swap"
				// Name the mechanism that ACTUALLY produced this change.
				// Hardcoding the K-gating string sent an operator (and me,
				// 2026-09-09) chasing composite-score swaps in a deployment
				// where sizing_mode=eligibility has no K, no ranking cut and
				// no swap threshold — every prod transition was mislabeled.
				reason := "K-gating composite-score swap"
				if s.cfg.SizingMode == "eligibility" {
					reason = "eligibility-set membership change"
				}
				if len(before) == 0 {
					txType = "bootstrap"
					reason = "first scoring round after restart — initial pool fill"
				}
				s.recordTransitionLocked(PoolTransition{
					At:         now,
					Type:       txType,
					Added:      added,
					Removed:    removed,
					PoolBefore: before,
					PoolAfter:  after,
					Reason:     reason,
					Source:     "scorer",
				})
			}
		}
		s.poolSet = newPoolSet
		s.poolMembers = newPoolMembers
		// Record the probing set we are about to render, so the next round can
		// tell whether it changed. Set here (not at the render call) so it stays
		// in lockstep with poolSet under the same lock and the same
		// not-deferred condition.
		s.lastUsPoolRendered = candidateSet
	}

	slog.Info("nodescorer: scored",
		"qualified", qualified,
		"total", len(nodes),
		"pool_changed", poolChanged,
		"us_pool_changed", usPoolChanged,
		"deferred", poolChangePending,
		"k_target", kTarget,
		"k_current", len(newPoolSet),
		"tier1_count", len(qualifiedNodes),
		"supply_limited", supplyLimited,
	)
	// Persist counters while still holding s.mu.Lock(). saveStateLocked
	// does NOT re-take the mutex (would self-deadlock on RWMutex semantics
	// — see persist.go).
	s.saveStateLocked()

	if reloadNeeded && !poolChangePending {
		// us-pool = the probing set (all currently-qualified candidates).
		// Mihomo url-test probes group members → keeping qualifiedNodes in
		// us-pool ensures fresh probe history for ALL candidates so
		// ranking remains accurate across rounds. Without this, OUT
		// nodes' probe history froze at whatever it was when they were
		// last in pool, locking the bootstrap selection in place.
		//
		// routingMembers = top-K subset (newPoolSet). Per-terminal HRW
		// uses ONLY this set, so production traffic stays on the K best
		// nodes. Identity preserved (each terminal still maps to one
		// stable node).
		//
		// When K-gating is disabled (kTarget=0), routingMembers=nil tells
		// the renderer to fall back to us-pool for per-terminal slicing —
		// legacy behavior (every us-pool member is a routing target).
		// usPoolAll = ALL pattern-matched candidates (not just qualifiedNodes).
		// Mihomo probes every member of us-pool; keeping all candidates here
		// ensures new/cold-start nodes accumulate probe history and can
		// graduate to qualifiedNodes in future rounds. Narrowing to
		// qualifiedNodes here caused a bootstrap deadlock: cold nodes never
		// got probed → ProbeCount stayed 0 → never qualified → never re-added.
		//
		// CAVEAT — us-pool is NOT only a probing set. `out (select)` lists it
		// first (groups.go), and perTerminalSlices ends with MATCH,us-pool, so
		// it is the catch-all for any source outside client_subnet. Being a
		// load-balance/consistent-hashing group, that FALLBACK traffic hashes
		// across every member — including candidates that are alive but not yet
		// measured. In-subnet terminals are unaffected (they route via fb-<ip>
		// built from routingMembers), so the exposure is fallback traffic only,
		// for roughly one scoring round until measurements arrive. This is the
		// deliberate price of the probing/routing split; narrowing usPoolAll to
		// avoid it would restore the bootstrap deadlock above.
		usPoolAll := append([]string(nil), candidates...)
		sort.Strings(usPoolAll)
		var routingMembers []string
		if kTarget > 0 {
			routingMembers = setToSortedSlice(newPoolSet)
		}
		members := newPoolMembers
		s.lastHotReload = now
		go s.hotReload(context.Background(), usPoolAll, routingMembers, members)
	}
}

// eligibilityPoolLocked builds newPoolSet under sizing_mode=eligibility:
// the routing set is every node that passes the existing liveness gate
// (Qualified) plus the two admission filters (not quarantined; not a fresh
// trial node unless already in the pool). No fixed-K target, no ranking cut,
// no fast-evict, no swap threshold, no fill-to-K post-pass — so there is no
// marginal slot for nodes to ping-pong over. See
// docs/design-eligibility-set-selection.md.
//
// min_pool_size is the only size number: if fewer than minPool nodes pass,
// admit the best available Qualified non-members up to the floor, preferring
// nodes ALREADY in the pool (incumbency) so a floor-bound pool does not
// re-pick its filler on EWMA noise every round (the one anti-flap piece the
// simplification kept). Below the floor, per-terminal HRW top-2 silently
// disables and one terminal's traffic splits across nodes by destination —
// so the floor protects the single-egress invariant, and a slow node beats
// no node.
//
// CALLER MUST HOLD s.mu.
func (s *Scorer) eligibilityPoolLocked(nodes []NodeHealth, newPoolSet map[string]bool, minPool int, now time.Time) {
	// Maintain the consecutive-alive-false counter BEFORE computing the
	// eligible set (computeEligibleSetLocked reads it via deadEvicted).
	// Done here in the mutating live path, not in the pure fn, so shadow
	// mode never perturbs it. A node newly crossing the DeadEvictRounds
	// threshold while in-pool is logged once (visible audit of the first
	// real dead-eviction, in lieu of a separate shadow round).
	for i := range nodes {
		name := nodes[i].Name
		st := s.state[name]
		if st == nil {
			continue
		}
		if nodes[i].Alive {
			st.deadRounds = 0
		} else {
			st.deadRounds++
			if s.cfg.DeadEvictRounds > 0 && st.deadRounds == s.cfg.DeadEvictRounds && s.poolSet[name] {
				slog.Warn("nodescorer: dead-evicting sustained-unreachable in-pool node",
					"node", name, "dead_rounds", st.deadRounds,
					"threshold", s.cfg.DeadEvictRounds, "fail_rate", nodes[i].FailRate,
					"note", "floor-subordinate: retained only if eviction would breach min_pool")
			}
		}
	}
	// Maintain the entry-hysteresis counter BEFORE computing the eligible set
	// (computeEligibleSetLocked reads it). A NON-MEMBER accumulates a run only
	// while it passes the admission quality gate; an in-pool node's counter is
	// held at 0 so that if it later exits it must re-earn entry from scratch
	// rather than being readmitted on the strength of a stale run.
	//
	// Live path only, so shadow mode never perturbs it — same reason
	// deadRounds is maintained here rather than in the pure function.
	for i := range nodes {
		st := s.state[nodes[i].Name]
		if st == nil {
			continue
		}
		switch {
		case s.poolSet[nodes[i].Name]:
			st.admitRuns = 0
		case nodes[i].Qualified && s.admissionRefusal(nodes[i]) == "":
			st.admitRuns++
		default:
			st.admitRuns = 0
		}
	}
	eligible := s.computeEligibleSetLocked(nodes, minPool, now)
	// Log REFUSALS only, and only for nodes that would otherwise be admissible
	// (Qualified, not quarantined, not dead-evicted, not already in pool).
	// Logging every candidate every round would be ~18 lines / 5min of noise;
	// a refusal is the actionable event — it is the line an operator reads when
	// asking "why is my new node not carrying traffic yet?". Done here in the
	// mutating path so computeEligibleSetLocked stays pure for shadow mode.
	// Log REFUSALS only, and only when the reason CHANGES. A refusal is the
	// actionable event — the line an operator reads when asking "why is my new
	// node not carrying traffic yet?" — but repeating it every round for every
	// held-out node is pure noise (with pattern discovery surfacing ~24
	// candidates that is thousands of lines/day). Transition-only logging keeps
	// the first refusal, and any later change of reason, while staying quiet in
	// steady state. Recovery (refusal → admitted) is visible via the pool
	// transition log, so it is not re-logged here.
	//
	// Done in this mutating path, not in computeEligibleSetLocked, so that
	// function stays pure for shadow mode.
	for i := range nodes {
		name := nodes[i].Name
		st := s.state[name]
		if eligible[name] || s.poolSet[name] || !nodes[i].Qualified {
			if st != nil {
				st.lastRefusal = ""
			}
			continue
		}
		if s.inQuarantine(name, now) || s.deadEvicted(name) {
			continue // refused for a reason other than quality; logged elsewhere
		}
		why := s.admissionRefusal(nodes[i])
		if st == nil {
			continue
		}
		if why != "" && why != st.lastRefusal {
			slog.Info("nodescorer: admission refused on quality",
				"node", name, "reason", why)
		}
		st.lastRefusal = why
	}
	for i := range nodes {
		name := nodes[i].Name
		st := s.state[name]
		if eligible[name] {
			st.strikes = 0
			st.okRuns++
			newPoolSet[name] = true
			nodes[i].InPool = true
		} else {
			// Diagnostic counters only; eligibility does not gate on them.
			st.okRuns = 0
			st.strikes++
		}
		nodes[i].Strikes = st.strikes
		nodes[i].OkRounds = st.okRuns
		st.health = nodes[i]
	}
}

// computeEligibleSetLocked is the PURE, side-effect-free membership function
// shared by the live eligibility path and the shadow-mode logger. It reads
// node health + s.poolSet/inQuarantine/deadEvicted/nodeLongEWMA and returns
// the set of nodes that should carry traffic; it mutates NOTHING (no
// newPoolSet, no nodes[].InPool, no st counters). Keeping it pure is what lets
// shadow mode run it in parallel with the live legacy decision without
// perturbing anything. Refusal LOGGING therefore lives in the mutating caller
// (eligibilityPoolLocked), not here. CALLER MUST HOLD s.mu.
// deadEvicted reports whether a node has been alive=false for at least
// cfg.DeadEvictRounds consecutive scoring rounds — i.e. mihomo has been
// unable to connect to it, sustained, not a single flap. Such a node is
// excluded from the normal routing set even if still Qualified via gstatic
// recentOk>0 (the soft-dead incumbent gap). It remains available to the
// redundancy-floor fill as a LAST resort (floor-subordinate), so a
// correlated outage falls to min_pool with dead nodes rather than below it.
// CALLER MUST HOLD s.mu.
func (s *Scorer) deadEvicted(name string) bool {
	if s.cfg.DeadEvictRounds <= 0 {
		return false // disabled
	}
	st := s.state[name]
	return st != nil && st.deadRounds >= s.cfg.DeadEvictRounds
}

// admissionRefusal reports why a NON-INCUMBENT node may not be admitted to
// the routing set, or "" when it passes. Incumbents never reach here — see
// computeEligibleSetLocked for why the gate is one-way.
//
// The three thresholds are the operator's EXISTING MaxRTTP95Ms / MaxJitterMs
// / MaxFailRate. They were configured, exposed via /api/status, and logged at
// startup, but until now no code read them: Plan-A disabled them because at
// 30s health-check granularity they churned pool membership every cycle
// (see scoreNode). That objection is about CONTINUOUS membership judgement.
// This is a ONE-TIME admission decision, judged once per node, so it cannot
// churn — and it is what makes it safe to admit a new node in ~5 minutes
// instead of after a 24h trial window. See
// docs/design-admission-quality-gate.md.
//
// A threshold of 0 means "not configured" and is skipped, so a deployment
// that never set one keeps the previous (unbounded) behavior for it rather
// than refusing everything.
//
// PURE: reads only the passed health + s.cfg. Callers may hold s.mu.
func (s *Scorer) admissionRefusal(h NodeHealth) string {
	// Measured-history gate (O1): never route users to a node before a real
	// measurement exists. scoreNode gives an alive node with no history
	// Qualified=true "benefit of doubt", which is what let the flapping-dead
	// ash nodes (US-01..05) get admitted, carry traffic for a round, reveal
	// fail=1.0 and drop — ~3 flaps/15min of pure churn. Such a node is still
	// probed via usPoolAll, so it can EARN admission; it just does not carry
	// users first. MinProbes (default 2) subsumes the older ProbeCount>0
	// condition this replaces.
	if min := s.cfg.MinProbes; min > 0 && h.ProbeCount < min {
		return fmt.Sprintf("probe_count %d < min_probes %d", h.ProbeCount, min)
	}
	if lim := s.cfg.MaxRTTP95Ms; lim > 0 && h.RTTP95Ms > lim {
		return fmt.Sprintf("p95 %dms > max_rtt_p95_ms %d", h.RTTP95Ms, lim)
	}
	if lim := s.cfg.MaxJitterMs; lim > 0 && h.JitterMs > lim {
		return fmt.Sprintf("jitter %dms > max_jitter_ms %d", h.JitterMs, lim)
	}
	if lim := s.cfg.MaxFailRate; lim > 0 && h.FailRate > lim {
		return fmt.Sprintf("fail_rate %.2f > max_fail_rate %.2f", h.FailRate, lim)
	}
	return ""
}

func (s *Scorer) computeEligibleSetLocked(nodes []NodeHealth, minPool int, now time.Time) map[string]bool {
	eligible := make(map[string]bool, len(nodes))
	// Bootstrap: empty pool (first round after a restart). Entry hysteresis is
	// skipped so the data plane gets a us-pool immediately rather than after
	// ReadmitStrikes rounds of no egress.
	bootstrap := len(s.poolSet) == 0
	for i := range nodes {
		name := nodes[i].Name
		// One-way door. An INCUMBENT (already carrying traffic) is judged only
		// on liveness — Qualified, not quarantined, not sustained-dead. A NEW
		// admission additionally has to clear the measured quality bar.
		//
		// The asymmetry IS the design: re-judging incumbents on quality every
		// round is exactly the Plan-A churn that eligibility mode exists to
		// remove. It also means this gate does NOT solve the "incumbent
		// degrades after admission" problem (ops doc U1) — that needs an exit
		// path, which is deliberately out of scope here.
		//
		// Replaces the former (!inTrial || poolSet) condition. trialDuration
		// asked "has this node existed long enough to trust?" as a proxy for
		// quality; this asks about quality directly, so a good node is usable
		// in ~5min instead of 24h AND a mid-grade bad node is refused for good
		// instead of admitted a day later. inTrial is still used by the legacy
		// K-gated path, where it guards RANKING competition that does not
		// exist here. See docs/design-admission-quality-gate.md.
		if !nodes[i].Qualified ||
			s.inQuarantine(name, now) ||
			s.deadEvicted(name) {
			continue
		}
		if s.poolSet[name] {
			eligible[name] = true
			continue
		}
		if s.admissionRefusal(nodes[i]) != "" {
			continue
		}
		// Entry hysteresis. A one-round quality sample is NOT evidence of
		// stability: mihomo keeps at most 10 url-test entries at a 30s
		// interval, so each 5-minute scoring round reads a FRESH, essentially
		// independent 10-probe window. A node with sustained ~30-40% loss
		// therefore clears fail_rate<=0.25 on a minority of rounds purely by
		// sampling luck, gets admitted, then trips the liveness gate
		// (recentOk==0) and exits — one hot-reload per crossing, indefinitely.
		//
		// This is the "新节点一轮幸运扫入" failure that
		// docs/design-eligibility-set-selection.md:120 lists as a MUST-KEEP
		// guard. That guard used to be `!inTrial` (a 24h window, hence
		// inherently multi-round); the 2026-09-07 admission-quality-gate
		// revision replaced it with a gate judged on ONE round and left
		// nothing spanning rounds. docs/design-eligibility-set-selection.md:222
		// budgeted for exactly this ("调 readmit_strikes 2–3") but the
		// eligibility path never read the knob — only the legacy K-gated path
		// does (see the okRuns check in the "auto" branch above).
		//
		// Incumbents are exempt (checked above), so this does NOT re-introduce
		// the per-round churn eligibility mode exists to remove: it is an
		// ENTRY condition only. Bootstrap is exempt so a cold start still
		// fills the pool in one round instead of stalling for ReadmitStrikes
		// rounds with no egress.
		if readmit := s.cfg.ReadmitStrikes; readmit > 1 && !bootstrap {
			if st := s.state[name]; st == nil || st.admitRuns < readmit {
				continue
			}
		}
		eligible[name] = true
	}

	// Redundancy floor with incumbency. Admit best-available Qualified
	// non-members up to minPool, sticking with the incumbent filler first so
	// the fill does not rotate on EWMA noise while floor-bound.
	if len(eligible) < minPool {
		type cand struct {
			name      string
			incumbent bool
			dead      bool // deadEvicted: floor-subordinate, only to hold the floor
			ewma      float64
		}
		candidates := make([]cand, 0, len(nodes))
		for i := range nodes {
			name := nodes[i].Name
			if nodes[i].Qualified && nodes[i].ProbeCount > 0 && !eligible[name] {
				candidates = append(candidates, cand{
					name:      name,
					incumbent: s.poolSet[name],
					dead:      s.deadEvicted(name),
					ewma:      s.nodeLongEWMA(name),
				})
			}
		}
		// Order: non-dead before dead (dead-evicted nodes are LAST resort —
		// Q4 floor-subordinate: a sustained-unreachable node is admitted only
		// if the pool would otherwise breach min_pool, and even then after
		// every live candidate). Within each dead/non-dead tier: incumbent
		// first (fill stability), then best EWMA. This also fixes Q3 thrash:
		// a dead-evicted node the main gate excluded is NOT pulled straight
		// back while any live candidate exists.
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].dead != candidates[j].dead {
				return !candidates[i].dead // non-dead first
			}
			if candidates[i].incumbent != candidates[j].incumbent {
				return candidates[i].incumbent // keep the incumbent filler
			}
			return candidates[i].ewma < candidates[j].ewma
		})
		for _, c := range candidates {
			if len(eligible) >= minPool {
				break
			}
			eligible[c.name] = true
		}

		// Cold-start last resort: genuine first boot (empty poolSet, no
		// node has probe history yet) leaves both the main gate and the
		// ProbeCount>0 fill empty — which would starve the data plane of a
		// us-pool entirely. Only here do we fall back to benefit-of-doubt
		// (alive + Qualified, ProbeCount==0) nodes, best-EWMA first, purely
		// to bring the pool up to the floor until real probes arrive. This
		// does NOT reopen the flap: it fires only when fewer than minPool
		// measured OR incumbent nodes exist, i.e. cold start, not steady
		// state.
		if len(eligible) < minPool {
			cold := make([]string, 0, len(nodes))
			for i := range nodes {
				name := nodes[i].Name
				// Availability floor: bypass TRIAL here (on cold start every
				// node is in-trial with no history, so respecting trial would
				// starve the pool for 24h) — but NEVER bypass QUARANTINE. A
				// node under rollback quarantine is an explicit operator "this
				// is bad" signal; a below-floor degraded pool is strictly
				// better than routing to a banned node. (Unlike the legacy
				// MinPool net, which was reached only with ProbeCount>0 nodes
				// and via a separate bootstrap path, this last-resort admits
				// ProbeCount==0 benefit-of-doubt nodes, so a freshly-restarted
				// quarantined node WOULD slip in without this guard — a
				// capability neither legacy nor pre-fix prod had.)
				if nodes[i].Qualified && !eligible[name] &&
					!s.inQuarantine(name, now) {
					cold = append(cold, name)
				}
			}
			// Deterministic order. Every cold-start candidate typically has NO
			// EWMA history, so nodeLongEWMA returns +Inf for all of them and a
			// bare EWMA comparator never reports "less" — leaving the winner to
			// sort.Slice's unstable pdqsort, i.e. an implementation detail.
			// That matters now that pattern discovery can surface a dozen
			// never-measured nodes at once: an arbitrary pick would then be
			// pinned by the incumbency preference in the measured fill above,
			// keeping a node that later measures badly while better siblings
			// wait. Incumbents first (fill stability), then EWMA where it
			// exists, then name as a total-order tiebreak.
			sort.Slice(cold, func(i, j int) bool {
				ini, inj := s.poolSet[cold[i]], s.poolSet[cold[j]]
				if ini != inj {
					return ini
				}
				ei, ej := s.nodeLongEWMA(cold[i]), s.nodeLongEWMA(cold[j])
				if ei != ej {
					return ei < ej
				}
				return cold[i] < cold[j]
			})
			for _, name := range cold {
				if len(eligible) >= minPool {
					break
				}
				eligible[name] = true
			}
		}
	}
	return eligible
}

// computePoolMembers maps each configured pool → the subset of the given
// us-pool set that currently passes all of the pool's required probes.
// CALLER MUST HOLD s.mu (reads s.probeResults).
func (s *Scorer) computePoolMembers(usPool map[string]bool) map[string][]string {
	if len(s.pools) == 0 {
		return map[string][]string{}
	}
	out := make(map[string][]string, len(s.pools))
	for _, p := range s.pools {
		if len(p.RequiresPassing) == 0 {
			continue // no gating → renderer falls back to full us-pool
		}
		var members []string
		for tag := range usPool {
			if s.nodePassesProbes(tag, p.RequiresPassing) {
				members = append(members, tag)
			}
		}
		sort.Strings(members)
		out[p.Name] = members
	}
	return out
}

// nodePassesProbes reports whether the node has a passing (ok=true) result
// for every named probe BY MAJORITY of its recent K=3 probe entries.
// Asymmetric hysteresis: a node needs >= 2 of 3 probes passing to be in
// the named pool; needs >= 2 of 3 failing to be kicked out. This means
// a single noisy CF response does NOT flip pool membership (and trigger
// a hot-reload). Missing history = not passing (conservative bootstrap).
// CALLER MUST HOLD s.mu.
func (s *Scorer) nodePassesProbes(tag string, required []string) bool {
	hist := s.probeOKHistory[tag]
	if hist == nil {
		return false
	}
	for _, name := range required {
		entries := hist[name]
		if len(entries) == 0 {
			return false
		}
		oks := 0
		for _, ok := range entries {
			if ok {
				oks++
			}
		}
		// Majority of recent entries must be OK. With window=3 this is
		// >= 2; with window=1 (first probe ever) the single result has
		// to be true. The /2-rounded bound keeps the rule consistent
		// when the window hasn't filled yet.
		if oks*2 <= len(entries) {
			return false
		}
	}
	return true
}

func (s *Scorer) scoreNode(tag string, pData map[string]interface{}, throughputBps float64) NodeHealth {
	h := NodeHealth{
		Name:          tag,
		ThroughputBps: throughputBps,
	}
	if idx := indexOf(tag, "/"); idx >= 0 {
		h.Sub = tag[:idx]
	}

	alive, _ := pData["alive"].(bool)
	h.Alive = alive

	// Extract probe history from extra["<probeURL>"].history
	history := s.extractHistory(pData)
	h.ProbeCount = len(history)

	if len(history) == 0 {
		if !alive {
			h.Reason = "dead"
			return h
		}
		// Bootstrap: an alive node with no probe history can't accumulate
		// data until mihomo's load-balance group includes it (mihomo only
		// probes group members). Marking it !qualified would lock it out
		// of the pool forever — vicious cycle. Give benefit of doubt so
		// it enters us-pool, gets probed, then real qualification kicks
		// in once history >= MinProbes.
		h.Qualified = true
		h.Reason = "no probe history yet (benefit of doubt)"
		return h
	}

	// Compute statistics.
	var delays []int
	failed := 0
	for _, d := range history {
		if d == 0 {
			failed++
		} else {
			delays = append(delays, d)
		}
	}
	h.FailRate = float64(failed) / float64(len(history))
	if len(delays) > 0 {
		h.LastRTTMs = history[0] // most recent
		sortInts(delays)
		h.RTTP50Ms = percentile(delays, 50)
		h.RTTP95Ms = percentile(delays, 95)
		h.JitterMs = h.RTTP95Ms - h.RTTP50Ms
	}

	// Qualify check.
	q := s.cfg
	if h.ProbeCount < q.MinProbes {
		// alive bit set by mihomo on last probe — only used as a hint
		// during bootstrap. Once we have probe history, we ignore it
		// (it flips with each 30s probe and would re-introduce noise).
		if !alive {
			h.Reason = "dead (no probe history)"
			return h
		}
		h.Qualified = true
		return h
	}
	// us-pool qualification rule (post Plan-A): a node is qualified if
	// it had at least one successful probe in the most recent 5 entries
	// of its probe history. RTT p50/p95/jitter/fail_rate thresholds are
	// NOT us-pool gates — they proved too noisy at 30s mihomo
	// health-check granularity (every scoring round the 5-of-10-probe-
	// window's noise floor flipped the qualified set, churning pool
	// membership and triggering hot-reload every cycle). Those metrics
	// are still computed and exposed via /api/nodes/health for
	// observability, just not gates.
	//
	// We deliberately ignore mihomo's `alive` bit here: alive reflects
	// ONLY the latest probe outcome, which flips at every 30s tick.
	// Reading it directly re-introduces the noise we set out to remove.
	// recent-success-in-last-5 smooths over single-probe blips.
	//
	// The smoothed-liveness gate works because:
	//   1. mihomo's load-balance internally skips a node whose latest
	//      probe failed (alive=false) for new flows — fast-path handles
	//      transient flaps without us touching yaml.
	//   2. A truly dead node accumulates failed probes; its WINDOW of
	//      last 5 entries shows zero successes, which DOES disqualify
	//      it via the recent-success count below.
	//   3. Named-pool membership (openai-pool, etc.) still gates on
	//      probe results — that's where CF-aware filtering lives.
	recentSpan := len(history)
	if recentSpan > 5 {
		recentSpan = 5
	}
	recentOk := 0
	for _, d := range history[len(history)-recentSpan:] {
		if d > 0 {
			recentOk++
		}
	}
	if recentOk == 0 {
		h.Reason = fmt.Sprintf("no successful probes in last %d", recentSpan)
		return h
	}
	// Catastrophic gate: a node with sustained ≥90% failure across the
	// full probe window (≥9 of 10) is unfit for the pool no matter how
	// low its p95 happens to be on the surviving probes. This is the ONE
	// hard threshold we re-introduce post-Plan-A — narrow enough to avoid
	// the noise that drove Plan A to remove all gates (single-probe blips
	// don't cross 0.9), wide enough to catch real "node is dead" cases
	// that compositeScore alone won't push out for hours via long EWMA.
	// Caught on 92 where cyberguard fail=1.0 sat in pool because its
	// score (1000) ranked below a slow-but-alive node (score 2870).
	// Smaller failures (fail<0.9) stay in the score-based ranking — the
	// quadratic fail term in compositeScore handles them smoothly.
	if h.FailRate >= hardFailThreshold {
		h.Reason = fmt.Sprintf("fail_rate %.2f >= %.2f (catastrophic)", h.FailRate, hardFailThreshold)
		return h
	}
	h.Qualified = true
	return h
}

func (s *Scorer) extractHistory(pData map[string]interface{}) []int {
	// mihomo stores per-url history under extra["<probeURL>"].history
	extra, _ := pData["extra"].(map[string]interface{})
	if extra == nil {
		return nil
	}
	urlData, _ := extra[s.probeURL].(map[string]interface{})
	if urlData == nil {
		// fallback: try any entry in extra
		for _, v := range extra {
			urlData, _ = v.(map[string]interface{})
			break
		}
	}
	if urlData == nil {
		return nil
	}
	rawHist, _ := urlData["history"].([]interface{})
	out := make([]int, 0, len(rawHist))
	for _, entry := range rawHist {
		m, _ := entry.(map[string]interface{})
		if m == nil {
			continue
		}
		d, _ := m["delay"].(float64)
		out = append(out, int(d))
	}
	return out
}

func (s *Scorer) updateThroughput(ctx context.Context, proxies map[string]map[string]interface{}) map[string]float64 {
	conns, err := s.fetchConnections(ctx)
	if err != nil {
		return nil
	}
	now := time.Now()
	// Snapshot per CONNECTION, not per node. Aggregating to the node here
	// is what broke the old implementation: node totals are computed over
	// the LIVE set, which churns, so the node-level difference conflated
	// "bytes moved" with "which connections happen to be open".
	curr := make(map[string]connBytes, len(conns))
	for _, c := range conns {
		id, _ := c["id"].(string)
		if id == "" {
			continue
		}
		chains, _ := c["chains"].([]interface{})
		if len(chains) == 0 {
			continue
		}
		// chains is leaf→root; chains[0] is the actual proxy node.
		node, _ := chains[0].(string)
		if node == "" {
			continue
		}
		up, _ := c["upload"].(float64)
		dn, _ := c["download"].(float64)
		curr[id] = connBytes{node: node, upload: int64(up), download: int64(dn)}
	}

	result := map[string]float64{}
	dt := now.Sub(s.connPrevT).Seconds()
	if s.connPrevT.IsZero() || dt <= 0 {
		s.connPrev = curr
		s.connPrevT = now
		return result
	}

	// Per-connection deltas, then sum per node. Counters are monotonic
	// within one connection's lifetime, so a negative delta can only mean
	// an id was reused for a different connection; treat that as new.
	byNode := map[string]int64{}
	for id, cb := range curr {
		prev, seen := s.connPrev[id]
		var d int64
		if !seen || cb.upload < prev.upload || cb.download < prev.download {
			// New this interval (or reused id): everything it has moved
			// was moved during this interval.
			d = cb.upload + cb.download
		} else {
			d = (cb.upload - prev.upload) + (cb.download - prev.download)
		}
		byNode[cb.node] += d
	}
	// Connections that closed during the interval are gone from `curr`, so
	// their last-poll-to-close bytes are unrecoverable from /connections
	// alone — a residual undercount bounded by one interval per closed
	// connection. What matters is that this no longer zeroes the whole
	// node: previously a single large connection closing made the node
	// total drop, the delta go negative, and the clamp discard every
	// still-live connection's traffic on that node as well.
	for node, total := range byNode {
		if total <= 0 {
			continue
		}
		if bps := math.Round(float64(total) / dt); bps > 0 {
			result[node] = bps
		}
	}
	s.connPrev = curr
	s.connPrevT = now
	return result
}

func (s *Scorer) hotReload(ctx context.Context, usPool, routingMembers []string, poolMembers map[string][]string) {
	if s.renderer == nil {
		return
	}
	outbounds := s.subscribe()
	data, err := s.renderer.RenderWithPools(outbounds, usPool, routingMembers, poolMembers)
	if err != nil {
		slog.Error("nodescorer: re-render failed", "err", err)
		return
	}
	if err := writeAtomic(s.renderer.Path(), data); err != nil {
		slog.Error("nodescorer: write config failed", "err", err)
		return
	}
	if err := s.mihomoHotReload(ctx); err != nil {
		slog.Error("nodescorer: hot-reload failed", "err", err)
		return
	}
	slog.Info("nodescorer: pool updated + hot-reloaded",
		"us_pool", len(usPool), "routing", len(routingMembers), "named_pools", len(poolMembers))
}

func (s *Scorer) mihomoHotReload(ctx context.Context) error {
	body := []byte(`{"path":"` + s.renderer.Path() + `"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		"http://"+s.apiAddr+"/configs?force=true",
		jsonBodyReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.apiSecret != "" {
		req.Header.Set("Authorization", "Bearer "+s.apiSecret)
	}
	resp, err := s.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("PUT /configs: HTTP %d", resp.StatusCode)
	}
	return nil
}

// filterCandidates returns the subset of outbound tags that:
//   - have a non-empty tag
//   - match the NodePattern regexp (empty pattern = match everything)
//   - exist as known entries in mihomo's /proxies map
//
// Using AllOutbounds() as source (not us-pool.all) means evicted nodes
// are still in the candidate set and can recover + be readmitted, and
// nodes from a newly-added subscription are discovered at all (see the
// closed-loop note in score()).
//
// NOTE: it does NOT filter to "node-bearing" outbounds — the doc comment used
// to claim that, but the body never checked it. In practice AllOutbounds only
// yields subscription outbounds (no groups or built-ins) and the /proxies
// existence check below rejects anything mihomo did not accept, so a
// non-node-bearing entry cannot survive. Stated explicitly because an empty
// NodePattern makes this function match every tag.
func filterCandidates(outbounds []subscribe.Outbound, nodePattern string, proxies map[string]map[string]interface{}) []string {
	var re *regexp.Regexp
	if nodePattern != "" {
		var err error
		re, err = regexp.Compile(nodePattern)
		if err != nil {
			re = nil
		}
	}
	var out []string
	for _, o := range outbounds {
		tag := o.Tag()
		if tag == "" {
			continue
		}
		if re != nil && !re.MatchString(tag) {
			continue
		}
		// Verify mihomo knows about this proxy (it may not if the subscription
		// hasn't been loaded yet or the proxy was removed).
		if _, ok := proxies[tag]; !ok {
			continue
		}
		out = append(out, tag)
	}
	return out
}

func (s *Scorer) fetchProxies(ctx context.Context) (map[string]map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+s.apiAddr+"/proxies", nil)
	if err != nil {
		return nil, err
	}
	if s.apiSecret != "" {
		req.Header.Set("Authorization", "Bearer "+s.apiSecret)
	}
	resp, err := s.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Proxies map[string]map[string]interface{} `json:"proxies"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Proxies, nil
}

func (s *Scorer) fetchConnections(ctx context.Context) ([]map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+s.apiAddr+"/connections", nil)
	if err != nil {
		return nil, err
	}
	if s.apiSecret != "" {
		req.Header.Set("Authorization", "Bearer "+s.apiSecret)
	}
	resp, err := s.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Connections []map[string]interface{} `json:"connections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Connections, nil
}
