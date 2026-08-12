// perterminal.go — "one terminal, one egress, with automatic failover".
//
// Each terminal IP gets a dedicated `fallback` proxy-group named "fb-<ip>"
// containing two members in HRW order: [primary, secondary]. SRC-IP-CIDR
// rules in the perterm sub-rule route the terminal to its fb-<ip> group.
// mihomo's fallback type tries members in order and uses the first one
// whose alive bit is true — so when primary dies, traffic auto-routes to
// secondary within mihomo's url-test interval, no leap-gateway intervention.
//
// Why not bare-node SRC-IP-CIDR (the previous design): SRC-IP-CIDR is a
// hard route — when its target proxy is alive=false, mihomo just fails
// the connection (no fallback). Per-terminal fallback groups give us
// mihomo's native alive-bit failover while preserving the per-terminal
// stable identity (same fb-<ip> group → same primary by default; only
// when primary is down does the egress IP change).
//
// HRW (rendezvous hashing) properties carry over: when the routing pool
// gains/loses a member, only the terminals whose top-2 changed get
// reassigned — most terminals keep their (primary, secondary) pair stable.

package mihomo

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"net"
	"sort"

	"github.com/leap-gateway/leap-gateway/internal/subscribe"
)

const (
	// maxPerTerminalHosts caps host enumeration so a misconfigured huge
	// subnet (e.g. /16 = 65k rules) can't blow up the config. /24 = 254 is
	// the normal FeiLian client-pool size; anything bigger than this many
	// hosts falls back to no per-terminal slicing.
	maxPerTerminalHosts = 1024

	// perTerminalFallbackDepth is the number of HRW-ordered members
	// included in each fb-<ip> fallback group. 2 = [primary, secondary]
	// is the minimum useful redundancy; larger values trade per-flap
	// rotation count for breadth (more candidates means more potential
	// IP changes during a multi-node failure).
	perTerminalFallbackDepth = 2

	// perTerminalGroupPrefix is the proxy-group naming convention. Operators
	// + the /api/pool/terminal endpoint rely on this prefix to enumerate
	// + look up terminal-specific groups.
	perTerminalGroupPrefix = "fb-"
)

// perTerminalMembers returns the routing-member pool used for per-terminal
// HRW assignment. Both perTerminalSlices and perTerminalFallbackGroups must
// call this so they always operate on the same set — if they diverge, rules
// can reference fb-<ip> groups that were never emitted (or vice versa),
// causing mihomo to reject the config with "proxy not found".
func (r *Renderer) perTerminalMembers(outbounds []subscribe.Outbound) []string {
	members := r.intersectWithPresent(r.routingMembers, outbounds)
	if len(members) == 0 {
		members = r.usPoolMembers(outbounds)
	}
	return members
}

// perTerminalSlices returns the `perterm` sub-rule body. Every host in the
// client subnet maps to its dedicated fb-<ip> fallback group, and the
// MATCH fallback at the end catches sources outside the subnet.
//
// Returns nil when there are no candidate routing members (validate path
// with empty outbounds, or unconfigured perTerminal); the caller then
// falls back to a single us-pool MATCH rule.
func (r *Renderer) perTerminalSlices(outbounds []subscribe.Outbound) []string {
	members := r.perTerminalMembers(outbounds)
	if len(members) == 0 {
		return nil
	}
	hosts := enumerateHosts(r.node.ClientSubnet, maxPerTerminalHosts)
	if len(hosts) == 0 {
		return nil
	}
	// Only emit fb-<ip> rules when we have enough members to form a fallback
	// group (perTerminalFallbackDepth=2). With <2 members perTerminalFallbackGroups
	// returns nil (no groups emitted), so the SRC-IP-CIDR rules would reference
	// nonexistent groups and mihomo rejects the config.
	if len(members) < perTerminalFallbackDepth {
		return nil
	}
	rules := make([]string, 0, len(hosts)+1)
	for _, ip := range hosts {
		rules = append(rules, fmt.Sprintf("SRC-IP-CIDR,%s/32,%s%s", ip, perTerminalGroupPrefix, ip))
	}
	rules = append(rules, "MATCH,"+usPool)
	return rules
}

// perTerminalFallbackGroups returns the fallback proxy-group definitions
// for every host in the client subnet. Each group has [primary, secondary]
// (HRW top-2) drawn from the routing-member pool. Returns nil when the
// pool is empty or the subnet can't be enumerated.
//
// The probe URL + interval mirror url_test config so all groups share
// the same liveness signal. lazy=true means the group only probes when
// it has active flows — keeps probe traffic proportional to actual
// terminal activity rather than the static subnet size.
func (r *Renderer) perTerminalFallbackGroups(outbounds []subscribe.Outbound, probeURL string, intervalSec int) []map[string]any {
	members := r.perTerminalMembers(outbounds)
	if len(members) < perTerminalFallbackDepth {
		// Need at least perTerminalFallbackDepth members to form a useful
		// fallback chain. perTerminalSlices checks the same condition, so
		// when this returns nil no SRC-IP-CIDR rules referencing fb-<ip>
		// will have been emitted either.
		return nil
	}
	hosts := enumerateHosts(r.node.ClientSubnet, maxPerTerminalHosts)
	if len(hosts) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(hosts))
	for _, ip := range hosts {
		ranked := hrwTopN(ip, members, perTerminalFallbackDepth)
		out = append(out, map[string]any{
			"name":     perTerminalGroupPrefix + ip,
			"type":     "fallback",
			"proxies":  ranked,
			"url":      probeURL,
			"interval": intervalSec,
			"lazy":     true,
		})
	}
	return out
}

// AssignmentForIP returns the HRW-ordered routing members for one
// terminal IP. Returns ([primary, secondary, ...], true) when the IP is
// inside the configured client_subnet AND the routing pool is non-empty,
// or (nil, false) when not applicable.
//
// Used by the /api/pool/terminal endpoint to surface "which node carries
// this terminal's traffic" without requiring the caller to re-implement
// HRW or read mihomo's runtime state.
//
// Reads routingMembers under stateMu so the API can see post-hot-reload
// pool composition. Without the lock the API lags by however long it
// takes a sub refresh to propagate (which can be never if no sub
// refresh fires after a scorer hot-reload).
func (r *Renderer) AssignmentForIP(ip string, outbounds []subscribe.Outbound) ([]string, bool) {
	r.stateMu.RLock()
	routingSnapshot := append([]string(nil), r.routingMembers...)
	r.stateMu.RUnlock()
	members := r.intersectWithPresent(routingSnapshot, outbounds)
	if len(members) == 0 {
		members = r.usPoolMembers(outbounds)
	}
	if len(members) == 0 {
		return nil, false
	}
	if !ipInSubnet(ip, r.node.ClientSubnet) {
		return nil, false
	}
	depth := perTerminalFallbackDepth
	if depth > len(members) {
		depth = len(members)
	}
	return hrwTopN(ip, members, depth), true
}

// intersectWithPresent filters wanted by membership in outbounds' tag set.
// Returns nil if outbounds is empty (validate path) so callers can fall
// back to their default member source. nil-safe on wanted.
func (r *Renderer) intersectWithPresent(wanted []string, outbounds []subscribe.Outbound) []string {
	if len(wanted) == 0 || len(outbounds) == 0 {
		return nil
	}
	present := make(map[string]bool, len(outbounds))
	for _, o := range outbounds {
		if t := o.Tag(); t != "" {
			present[t] = true
		}
	}
	out := make([]string, 0, len(wanted))
	for _, w := range wanted {
		if present[w] {
			out = append(out, w)
		}
	}
	return out
}

// enumerateHosts returns the usable host IPs of a CIDR (excludes network +
// broadcast for IPv4 prefixes shorter than /31). Returns nil for non-CIDR
// or when the host count exceeds limit.
func enumerateHosts(cidr string, limit int) []string {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil
	}
	ip4 := ipnet.IP.To4()
	if ip4 == nil {
		return nil // IPv6 client pools not supported for per-terminal slicing
	}
	ones, bits := ipnet.Mask.Size()
	hostBits := bits - ones
	if hostBits <= 0 {
		return nil
	}
	count := 1 << uint(hostBits)
	base := binary.BigEndian.Uint32(ip4)
	var out []string
	for i := 0; i < count; i++ {
		if hostBits >= 2 && (i == 0 || i == count-1) {
			continue
		}
		if len(out) >= limit {
			return nil
		}
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], base+uint32(i))
		out = append(out, net.IP(b[:]).String())
	}
	return out
}

// ipInSubnet returns true when ip is inside cidr.
func ipInSubnet(ip, cidr string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	return ipnet.Contains(parsed)
}

// splitmix64 is the finalizer applied to the FNV output in hrwTopN.
//
// FNV-1a has weak avalanche: it is a multiply-xor over bytes with no
// final mixing step, so structured inputs leave visible correlation in
// the high bits that decide the HRW argmax. Measured on 92's live
// 8-member routing pool over the whole 254-terminal /24, member[0] came
// out as [5 9 17 17 23 59 62 62] — a 12.4x spread between the least and
// most loaded egress, chi2=136 against uniform.
//
// This is NOT about the CJK/emoji node names, which was the initial
// suspicion. An ASCII control set (n1..n8) skewed 11.2x with one node
// taking 52.8% of terminals, i.e. worse. The skew depends
// unpredictably on the particular name set.
//
// splitmix64's finalizer fixes it: the same measurement becomes
// [22 26 31 32 34 35 36 38], 1.7x, chi2=6.3. It also removes the
// "K=3 needs a lucky triple" problem — across all 56 three-member
// subsets of that pool, subsets putting >=45% of terminals on one node
// drop from 40/56 to 0/56, and the worst-case single-node share drops
// from 52.0% to 40.6%.
//
// Rejected alternative: hashing member-then-ip instead of ip-then-member.
// It is dramatically worse, not better — 74.8% of terminals landed on a
// single node (chi2=981), because the trailing ip bytes are nearly
// identical across a /24 and FNV's tail dominates the result.
func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// hrwTopN returns the top-N members for an IP in HRW score order
// (highest score first = primary). Deterministic; the same (ip, members)
// always produces the same ordering. Adding/removing one member only
// reshuffles entries containing that member; others stay stable.
//
// NOTE: changing the hash changes every terminal's assignment exactly
// once. The splitmix64 finalizer was added 2026-08-12 and reshuffled all
// 254 terminals in one step; that is a one-time cost, not a recurring
// one, and the HRW stability property above is unaffected.
func hrwTopN(ip string, members []string, n int) []string {
	if n <= 0 || len(members) == 0 {
		return nil
	}
	type scored struct {
		name  string
		score uint64
	}
	scoredMembers := make([]scored, len(members))
	for i, m := range members {
		h := fnv.New64a()
		_, _ = h.Write([]byte(ip))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(m))
		scoredMembers[i] = scored{m, splitmix64(h.Sum64())}
	}
	sort.Slice(scoredMembers, func(i, j int) bool {
		return scoredMembers[i].score > scoredMembers[j].score
	})
	if n > len(scoredMembers) {
		n = len(scoredMembers)
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = scoredMembers[i].name
	}
	return out
}

// hrwPick is preserved as a thin wrapper over hrwTopN(..., 1) for any
// existing call site (e.g. tests). Same behavior as before — first HRW.
func hrwPick(ip string, members []string) string {
	r := hrwTopN(ip, members, 1)
	if len(r) == 0 {
		return ""
	}
	return r[0]
}
