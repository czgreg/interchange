package nodescorer

import (
	"bytes"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		x := a[i]
		j := i - 1
		for j >= 0 && a[j] > x {
			a[j+1] = a[j]
			j--
		}
		a[j+1] = x
	}
}

func percentile(sorted []int, p int) int {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	rank := float64(p) / 100.0 * float64(len(sorted)-1)
	lo := int(rank)
	hi := lo + 1
	if hi >= len(sorted) {
		return sorted[lo]
	}
	frac := rank - float64(lo)
	return int(float64(sorted[lo]) + frac*float64(sorted[hi]-sorted[lo]))
}

func poolSetsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// setToSortedSlice flattens a set to a sorted slice (deterministic render
// output → stable config diffs).
func setToSortedSlice(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// poolMembersEqual compares two pool-name → member-slice maps. Member
// slices are compared order-insensitively via length + set membership.
func poolMembersEqual(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, am := range a {
		bm, ok := b[name]
		if !ok || len(am) != len(bm) {
			return false
		}
		bset := make(map[string]bool, len(bm))
		for _, x := range bm {
			bset[x] = true
		}
		for _, x := range am {
			if !bset[x] {
				return false
			}
		}
	}
	return true
}

func indexOf(s, sep string) int {
	return strings.Index(s, sep)
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// jsonBodyReader returns an io.Reader for a JSON body, used to avoid
// importing bytes in scorer.go which is already large.
func jsonBodyReader(b []byte) io.Reader {
	return bytes.NewReader(b)
}

// compositeScore is the lower-is-better quality metric used to rank
// qualified candidates for top-K pool selection. Combines latency tail,
// jitter, and probe failure rate; passive RST/short-conn rate is folded
// in when available (heavier weight — passive failures map directly to
// user-perceived broken sessions).
//
// A node with no probe history (ProbeCount==0) is "qualified by benefit
// of doubt" in scoreNode but gets a sentinel score so it ranks below ANY
// node that has real probe data — otherwise zero-history nodes sweep the
// top of the ranking with a free score=0 and crowd out actually-measured
// good nodes. The sentinel is finite (not Inf) so two zero-history nodes
// remain comparable via stable sort (insertion order).
//
// fail_rate uses a quadratic term so "completely broken" outranks "just
// slow" by an order of magnitude, preventing the ranking inversion
// observed on 92: cyberguard fail=1.0 was ranking BELOW fishcloud
// p95=2858ms because 1000 (linear fail term) < 2870 (latency term).
// With the quadratic, fail=1.0 contributes 5000 → above any plausible
// healthy-but-slow node. Linear sub-thresholds still discriminate small
// fail differences smoothly (fail=0.1 → 50; fail=0.3 → 450).
//
// Calibration intent (when ProbeCount > 0):
//   - p95 weight 1.0 (the user-perceived tail)
//   - jitter weight 2.0 (a flapping node hurts experience more than a
//     consistently slower one with the same p95)
//   - probe fail_rate² × 5000 (so fail=1.0 = 5000, fail=0.5 = 1250,
//     fail=0.25 = 312 — small failures barely move score, complete
//     death dominates)
//   - passive fail_rate × 500 (real-traffic broken connections — half
//     the weight of probe fail_rate because passive sample is smaller)
func compositeScore(h NodeHealth) float64 {
	if h.ProbeCount == 0 {
		// No measurement yet — rank below any real-data node. 1e9 is
		// large enough to lose to any reasonable composite (a node with
		// p95=10000ms and fail=1.0 still scores ~15000) and small enough
		// that insertion-order stable sort breaks ties deterministically.
		return 1e9
	}
	score := float64(h.RTTP95Ms) + 2*float64(h.JitterMs) + 5000*h.FailRate*h.FailRate
	if h.Passive != nil && h.Passive.ClosedWindow > 0 {
		score += 500 * h.Passive.FailRate
	}
	return score
}

// computeK returns the target us-pool size from the operator's pool-sizing
// config and the current Tier-1 supply. Returns (0, false) when TActive=0
// (K-gating disabled — legacy "everyone in pool" behavior preserved).
//
//	K = clamp(
//	  ⌈T × surge / cap⌉,                                 // K_demand
//	  max(KMin, ⌈T / (cap × 1.5)⌉ + 1),                  // K_min(T) — failure resilience
//	  min(KMax, qualifiedCount),                          // supply ceiling
//	)
//
// supplyLimited is true when |Tier1| was the binding ceiling — used by
// /api/status to surface "want K=N, have only M trusted" advisories.
func computeK(p config.PoolSizingConfig, qualifiedCount int) (k int, supplyLimited bool) {
	if p.TActive <= 0 {
		return 0, false
	}
	cap := p.CapPerNode
	if cap <= 0 {
		cap = 10
	}
	surge := p.Surge
	if surge <= 0 {
		surge = 1.5
	}
	kMin := p.KMin
	if kMin <= 0 {
		kMin = 2
	}
	kMax := p.KMax
	if kMax <= 0 {
		kMax = 12
	}

	kDemand := int(math.Ceil(float64(p.TActive) * surge / float64(cap)))
	// K_min(T) — survive 1-node failure with 1.5× surge headroom on
	// remaining members. Ensures T/(K-1) ≤ cap × 1.5 → K ≥ T/(cap×1.5) + 1.
	kFloor := int(math.Ceil(float64(p.TActive)/(float64(cap)*1.5))) + 1
	if kFloor < kMin {
		kFloor = kMin
	}

	k = kDemand
	if k < kFloor {
		k = kFloor
	}
	if k > kMax {
		k = kMax
	}
	if k > qualifiedCount {
		return qualifiedCount, true
	}
	return k, false
}
