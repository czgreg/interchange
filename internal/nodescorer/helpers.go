package nodescorer

import (
	"bytes"
	"io"
	"os"
	"sort"
	"strings"
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
