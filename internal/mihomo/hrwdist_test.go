package mihomo

// Distribution regression tests for hrwTopN.
//
// These guard the splitmix64 finalizer added 2026-08-12. Without it, FNV-1a's
// weak avalanche skewed member[0] across the live 254-terminal /24 by 12.4x
// (worst egress carried 62 terminals, best carried 5) — and an ASCII control
// set was worse still at 11.2x with one node taking 52.8%. A regression here
// silently overloads one egress node, which is invisible in aggregate metrics
// because the pool as a whole looks fine.

import (
	"fmt"
	"sort"
	"testing"
)

// The live 8-member routing pool on 92 as of 2026-08-12, verbatim. Kept
// exact (CJK + emoji + the " · " separator) because the skew is
// name-set-dependent: paraphrasing these would not reproduce it.
var liveRoutingPool = []string{
	"Hutao/美国_BGP_A",
	"Hutao/美国_BGP_B",
	"Hutao/美国_BGP_C",
	"Hutao/美国_BGP_D",
	"Hutao/美国_BGP_E",
	"ash/🇺🇸US-02 · AWS-SG",
	"fishcloud/🇺🇸 美国01 境外中转",
	"fishcloud/🇺🇸 美国02 境外中转",
}

// firstMemberCounts returns how many of the /24's 254 usable terminals get
// each member as their primary (member[0]).
func firstMemberCounts(members []string, depth int) map[string]int {
	out := map[string]int{}
	for n := 1; n <= 254; n++ {
		ranked := hrwTopN(fmt.Sprintf("10.8.12.%d", n), members, depth)
		if len(ranked) > 0 {
			out[ranked[0]]++
		}
	}
	return out
}

func spread(counts map[string]int, members []string) (min, max int, ratio float64) {
	min, max = 1 << 30, 0
	for _, m := range members {
		c := counts[m]
		if c < min {
			min = c
		}
		if c > max {
			max = c
		}
	}
	if min > 0 {
		ratio = float64(max) / float64(min)
	}
	return
}

// The headline property: no egress may carry a wildly disproportionate
// share. 3.0x is a deliberately loose bound — measured is 1.7x, and the
// pre-fix value was 12.4x, so this catches a real regression without
// failing on hash-dependent noise.
func TestHRWTopN_PrimaryDistributionIsBalanced(t *testing.T) {
	counts := firstMemberCounts(liveRoutingPool, 2)
	min, max, ratio := spread(counts, liveRoutingPool)
	if ratio > 3.0 {
		t.Errorf("member[0] spread %dx (min=%d max=%d) exceeds 3.0x; counts=%v",
			int(ratio), min, max, counts)
	}
	// No single egress may take more than a quarter of all terminals with
	// 8 members (uniform is 12.5%). Pre-fix worst was 24.4%, which passed
	// this bound — the ratio check above is what catches that case.
	if share := 100 * float64(max) / 254; share > 25.0 {
		t.Errorf("worst egress carries %.1f%% of terminals, over the 25%% bound", share)
	}
}

// Every member must carry at least some traffic. A member[0] count of 0
// means that egress is only ever a secondary, so the pool is effectively
// smaller than K.
func TestHRWTopN_EveryMemberIsSomeonesPrimary(t *testing.T) {
	counts := firstMemberCounts(liveRoutingPool, 2)
	for _, m := range liveRoutingPool {
		if counts[m] == 0 {
			t.Errorf("member %q is never a primary; counts=%v", m, counts)
		}
	}
}

// The K=3 property that makes pool-size reduction viable: no three-member
// subset may concentrate 45%+ of terminals on one node. Pre-fix, 40 of the
// 56 subsets did, with a worst case of 52%. pool_mode=auto rotates members
// every scoring interval, so "the current triple happens to be balanced"
// is not a durable property — every subset has to be acceptable.
func TestHRWTopN_AllThreeMemberSubsetsAreAcceptable(t *testing.T) {
	worst := 0.0
	worstSet := []string{}
	bad := 0
	total := 0
	for i := 0; i < len(liveRoutingPool); i++ {
		for j := i + 1; j < len(liveRoutingPool); j++ {
			for k := j + 1; k < len(liveRoutingPool); k++ {
				total++
				tri := []string{liveRoutingPool[i], liveRoutingPool[j], liveRoutingPool[k]}
				counts := firstMemberCounts(tri, 2)
				_, max, _ := spread(counts, tri)
				share := 100 * float64(max) / 254
				if share > worst {
					worst, worstSet = share, tri
				}
				if share >= 45.0 {
					bad++
				}
			}
		}
	}
	if total != 56 {
		t.Fatalf("expected 56 subsets of 8 choose 3, got %d", total)
	}
	if bad != 0 {
		t.Errorf("%d/56 three-member subsets concentrate >=45%% on one node "+
			"(was 40/56 before the splitmix64 finalizer)", bad)
	}
	if worst > 45.0 {
		t.Errorf("worst subset share %.1f%% on %v exceeds 45%%", worst, worstSet)
	}
}

// HRW's defining property, which the finalizer must not break: removing a
// member may only reassign the terminals that pointed at it. Everyone
// else's primary stays put. Without this, one node leaving the pool
// reshuffles every terminal's egress IP.
func TestHRWTopN_RemovingMemberOnlyMovesItsOwnTerminals(t *testing.T) {
	full := liveRoutingPool
	victim := full[3]
	reduced := make([]string, 0, len(full)-1)
	for _, m := range full {
		if m != victim {
			reduced = append(reduced, m)
		}
	}
	moved, movedWrongly := 0, 0
	for n := 1; n <= 254; n++ {
		ip := fmt.Sprintf("10.8.12.%d", n)
		before := hrwTopN(ip, full, 1)[0]
		after := hrwTopN(ip, reduced, 1)[0]
		if before == after {
			continue
		}
		moved++
		if before != victim {
			movedWrongly++
		}
	}
	if movedWrongly != 0 {
		t.Errorf("%d terminals whose primary was NOT the removed member changed anyway — "+
			"HRW stability is broken", movedWrongly)
	}
	if moved == 0 {
		t.Error("removing a member moved nobody; the fixture is not exercising the property")
	}
}

// Determinism: same inputs, same output, every call. The whole per-terminal
// stickiness design rests on this.
func TestHRWTopN_IsDeterministic(t *testing.T) {
	for n := 1; n <= 254; n += 37 {
		ip := fmt.Sprintf("10.8.12.%d", n)
		first := hrwTopN(ip, liveRoutingPool, 3)
		for i := 0; i < 5; i++ {
			again := hrwTopN(ip, liveRoutingPool, 3)
			if len(first) != len(again) {
				t.Fatalf("%s: length varies between calls", ip)
			}
			for x := range first {
				if first[x] != again[x] {
					t.Fatalf("%s: ordering varies between calls: %v vs %v", ip, first, again)
				}
			}
		}
	}
}

// Member order in the input must not affect the result — the routing pool
// arrives from a map iteration in some call paths, so a sort-order
// dependency would make assignments unstable across renders.
func TestHRWTopN_IndependentOfInputOrder(t *testing.T) {
	shuffled := append([]string(nil), liveRoutingPool...)
	sort.Sort(sort.Reverse(sort.StringSlice(shuffled)))
	for n := 1; n <= 254; n++ {
		ip := fmt.Sprintf("10.8.12.%d", n)
		a := hrwTopN(ip, liveRoutingPool, 2)
		b := hrwTopN(ip, shuffled, 2)
		for x := range a {
			if a[x] != b[x] {
				t.Fatalf("%s: result depends on input order: %v vs %v", ip, a, b)
			}
		}
	}
}
