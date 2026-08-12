// ewma.go — exponentially-weighted moving averages for node health.
//
// Two windows tracked per node:
//
//   short EWMA (4h half-life)  — drives EVICT decisions. Catches recent
//                                degradation fast: a node that was fine
//                                yesterday but failed for 2h continuously
//                                shows up here at half-strength already.
//
//   long  EWMA (24h half-life) — drives PROMOTE decisions. Conservative:
//                                a "recently recovered" node carries
//                                yesterday's bad data weighted heavily,
//                                so it stays out of pool until the long
//                                window forgets the bad period. Matches
//                                the operator intuition "I want to see
//                                a node be stable for a day before I
//                                trust it with traffic".
//
// Both EWMAs handle irregular sampling intervals: the smoothing factor
// alpha is computed from elapsed time vs half-life, so a 5-min gap and
// a 30-min gap are weighted correctly. (Standard time-aware EWMA per
// e.g. https://en.wikipedia.org/wiki/Moving_average#Modified_moving_average)
//
// The metric tracked is the same compositeScore used by current K-gating
// (p95 + 2*jitter + 1000*fail_rate + passive contribution), so plumbing
// downstream stays simple.

package nodescorer

import (
	"math"
	"sort"
	"time"
)

const (
	// shortHalfLife is the time after which a single observation's weight
	// in the short EWMA drops to 50%. Chosen to react to BGP / airport
	// quality shifts on the same timescale a user would notice them.
	shortHalfLife = 4 * time.Hour

	// longHalfLife is the slow trust signal — drives promote decisions.
	// 24h means yesterday's bad period still counts at 50% today; a
	// "recently recovered" node has to be good for a full day before
	// the long EWMA forgets the bad period.
	longHalfLife = 24 * time.Hour

	// trialDuration: nodes with shorter probe history can't be promoted
	// by EWMA logic. Below this, promotion only happens via:
	//   (a) bootstrap (pool empty)
	//   (b) emergency_promote_chain (operator-pre-approved)
	//   (c) operator yaml edit
	// Forces new nodes to "earn" their slot via observation, prevents
	// a fresh subscription of unknown nodes from sweeping the top by
	// luck of initial probe.
	trialDuration = 24 * time.Hour

	// evictShortEWMAFactor is how much worse than the pool's own median
	// short EWMA a member may get before it is evicted immediately,
	// without waiting for the 24h long window to notice.
	//
	// Relative to the pool median rather than an absolute ms value so the
	// gate travels across subscription tiers: when every node is slow the
	// median rises with them and nothing is evicted for being normal.
	//
	// 2.0 is the deliberately conservative starting point. At the observed
	// pool median short EWMA of ~250-400 that puts the trigger around
	// 500-800, i.e. roughly "twice as bad as typical", which no healthy
	// node in the measured population reaches. Lower it only with evidence
	// — the failure mode of a too-tight factor is pool churn, which is what
	// the long-window conservatism exists to prevent.
	evictShortEWMAFactor = 2.0

	// noMeasurementScore is the compositeScore sentinel for a node with no
	// probe history yet (see helpers.go). It must never enter the EWMA: it
	// is ~6 orders of magnitude larger than a real composite (~200–2800),
	// so once smoothed in it dominates the value for days at the 24h
	// half-life and the promote ranking + SwapThresholdScore gate degrade
	// to noise. ewmaPoint.update rejects observations >= this, and
	// sanitizeEWMA scrubs any value the bug already wrote to disk.
	noMeasurementScore = 1e9
)

// nodeEWMA carries the per-node EWMA state. Persisted in the state
// file (v3 schema, optional fields — older state files just keep the
// EWMA as zero, which behaves like "no signal yet" and falls back to
// raw compositeScore).
type nodeEWMA struct {
	Short        ewmaPoint `json:"short"`
	Long         ewmaPoint `json:"long"`
	FirstSeenAt  time.Time `json:"first_seen_at,omitempty"`
}

// ewmaPoint is one window's smoothed metric value + when it was last
// updated (used to compute the time-aware alpha for the next update).
type ewmaPoint struct {
	Value     float64   `json:"value"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// update applies a new observation. observed is the round's raw
// compositeScore; halfLife determines how aggressively the new
// observation overrides the prior smoothed value. First call (zero
// UpdatedAt) seeds the EWMA at the observed value.
func (p *ewmaPoint) update(observed float64, now time.Time, halfLife time.Duration) {
	// Reject the no-measurement sentinel. score() already guards the feed
	// on ProbeCount==0, but this is the last line of defense: a single
	// sentinel observation poisons the window for days (see
	// noMeasurementScore). A no-data round simply leaves the EWMA untouched.
	if observed >= noMeasurementScore {
		return
	}
	if p.UpdatedAt.IsZero() {
		p.Value = observed
		p.UpdatedAt = now
		return
	}
	elapsed := now.Sub(p.UpdatedAt)
	if elapsed <= 0 {
		// Out-of-order or same-instant update — just refresh time, no math.
		p.UpdatedAt = now
		return
	}
	// alpha = 1 - exp(-ln(2) * elapsed / halfLife). For elapsed=halfLife,
	// alpha = 0.5 (50% weight on the new observation).
	alpha := 1.0 - math.Exp(-math.Ln2*float64(elapsed)/float64(halfLife))
	p.Value = alpha*observed + (1.0-alpha)*p.Value
	p.UpdatedAt = now
}

// updateNodeEWMA refreshes both short and long EWMAs for a node with
// the given current-round observation. Returns the updated state for
// caller convenience (so they can read .Long for promote decisions).
//
// CALLER MUST HOLD s.mu.
func (s *Scorer) updateNodeEWMA(name string, observed float64, now time.Time) *nodeEWMA {
	if s.ewma == nil {
		s.ewma = map[string]*nodeEWMA{}
	}
	e, ok := s.ewma[name]
	if !ok {
		e = &nodeEWMA{FirstSeenAt: now}
		s.ewma[name] = e
	}
	e.Short.update(observed, now, shortHalfLife)
	e.Long.update(observed, now, longHalfLife)
	return e
}

// inTrial reports whether the named node is still inside its 24h trial
// window (history shorter than trialDuration). Trial nodes are excluded
// from EWMA-based promote — they need to be observed for a full day
// before the system trusts their long EWMA. CALLER MUST HOLD s.mu.
func (s *Scorer) inTrial(name string, now time.Time) bool {
	e, ok := s.ewma[name]
	if !ok {
		return true // never seen → maximally untrusted
	}
	if e.FirstSeenAt.IsZero() {
		return true
	}
	return now.Sub(e.FirstSeenAt) < trialDuration
}

// nodeLongEWMA returns the long-window smoothed compositeScore for
// promote decisions. Returns +Inf when the node has no EWMA history
// yet (so it ranks below any observed node).
func (s *Scorer) nodeLongEWMA(name string) float64 {
	e, ok := s.ewma[name]
	if !ok || e.Long.UpdatedAt.IsZero() {
		return math.Inf(1)
	}
	return e.Long.Value
}

// nodeShortEWMA returns the short-window smoothed compositeScore for
// evict decisions. Same +Inf semantics for unobserved nodes.
func (s *Scorer) nodeShortEWMA(name string) float64 {
	e, ok := s.ewma[name]
	if !ok || e.Short.UpdatedAt.IsZero() {
		return math.Inf(1)
	}
	return e.Short.Value
}

// poolShortEWMAMedian is the median short EWMA across current pool
// members, and the reference point for fast eviction.
//
// Median rather than mean: the mean is dragged by exactly the degraded
// member we are trying to detect, which raises the threshold and hides it.
// Nodes with no short-window signal yet (+Inf) are skipped rather than
// treated as infinitely bad — they would push the median to +Inf and
// disable eviction entirely on a freshly-restarted gateway.
//
// Returns 0 when fewer than 3 members have a usable signal: with 1-2
// samples "median" is not a population statistic, and evicting against it
// risks removing a healthy node because its one peer happens to be fast.
// Callers treat 0 as "do not evict this round".
//
// CALLER MUST HOLD s.mu.
func (s *Scorer) poolShortEWMAMedian() float64 {
	vals := make([]float64, 0, len(s.poolSet))
	for name := range s.poolSet {
		v := s.nodeShortEWMA(name)
		if math.IsInf(v, 1) {
			continue
		}
		vals = append(vals, v)
	}
	if len(vals) < 3 {
		return 0
	}
	sort.Float64s(vals)
	mid := len(vals) / 2
	if len(vals)%2 == 1 {
		return vals[mid]
	}
	return (vals[mid-1] + vals[mid]) / 2
}

// poolFillCeiling is the maximum compositeScore a node may carry and still
// be admitted by the kTarget fill post-pass. Derived from the median
// composite of the CURRENT pool so it adapts to the tier, times the same
// factor used for fast eviction (a node too slow to keep is also too slow
// to add).
//
// Returns 0 ("no ceiling") when the current pool has fewer than 3 measured
// members — during bootstrap or a supply collapse there is no meaningful
// population to compare against, and refusing to fill would leave the data
// plane emptier than it needs to be.
//
// CALLER MUST HOLD s.mu.
func (s *Scorer) poolFillCeiling(nodes []NodeHealth) float64 {
	vals := make([]float64, 0, len(s.poolSet))
	for i := range nodes {
		if !s.poolSet[nodes[i].Name] || nodes[i].ProbeCount == 0 {
			continue
		}
		c := compositeScore(nodes[i])
		if c >= noMeasurementScore {
			continue
		}
		vals = append(vals, c)
	}
	if len(vals) < 3 {
		return 0
	}
	sort.Float64s(vals)
	mid := len(vals) / 2
	med := vals[mid]
	if len(vals)%2 == 0 {
		med = (vals[mid-1] + vals[mid]) / 2
	}
	return med * evictShortEWMAFactor
}
