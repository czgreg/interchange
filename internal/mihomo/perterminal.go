// perterminal.go — "one terminal, one egress" slice generation.
//
// When load_balance.per_terminal is on, each client terminal's whitelisted
// traffic must exit through a single, stable node so a logical session
// (e.g. ChatGPT spread across chatgpt.com / chat.openai.com / ws.chatgpt.com)
// presents ONE egress IP and doesn't trip OpenAI/Cloudflare anomaly checks.
//
// We render the node.client_subnet into per-/32 SRC-IP-CIDR rules under the
// `perterm` sub-rule, each pinned to a us-pool member chosen by rendezvous
// (highest-random-weight) hashing. HRW means a node leaving the qualified
// set only reshuffles the terminals that were on THAT node — every other
// terminal keeps its egress. The list ends with MATCH,us-pool so any source
// outside the subnet (or if enumeration is skipped) still works.

package mihomo

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"net"

	"github.com/leap-gateway/leap-gateway/internal/subscribe"
)

// maxPerTerminalHosts caps host enumeration so a misconfigured huge subnet
// (e.g. /16 = 65k rules) can't blow up the config. /24 = 254 is the normal
// FeiLian client-pool size; anything bigger than this many hosts falls back
// to no per-terminal slicing (operator should narrow client_subnet).
const maxPerTerminalHosts = 1024

// perTerminalSlices returns the `perterm` sub-rule body using the routing
// member set: when r.routingMembers is set (K-gating active) only those
// nodes carry production traffic; otherwise falls back to the full us-pool
// member set (legacy / no K-gating). The fallback target ("us-pool") in
// the MATCH rule keeps routing functional for sources outside the
// enumerated client_subnet.
//
// We intersect the routing set with present outbound tags before emitting:
// during --validate the outbounds list is empty (RenderOnly(nil)), and
// during normal operation a stale routing list may reference nodes that
// were dropped from a subscription. Either way mihomo refuses configs
// that route to nonexistent proxy names — silent fallback to the present
// us-pool intersection avoids a hard fail.
func (r *Renderer) perTerminalSlices(outbounds []subscribe.Outbound) []string {
	members := r.intersectWithPresent(r.routingMembers, outbounds)
	if len(members) == 0 {
		members = r.usPoolMembers(outbounds)
	}
	if len(members) == 0 {
		return nil
	}
	return buildSliceRules(r.node.ClientSubnet, members, usPool)
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

// perTerminalSlicesForPool returns the `perterm-<poolName>` sub-rule body
// using only the pool-specific qualified members (those that passed the
// pool's site probes). Falls back to the routing set (top-K us-pool when
// K-gating is on) when the pool has no members yet.
func (r *Renderer) perTerminalSlicesForPool(poolName string, outbounds []subscribe.Outbound) []string {
	members, ok := r.poolMembers[poolName]
	if !ok || len(members) == 0 {
		// No probe results yet: fall back through routingMembers
		// (top-K when K-gating active) → us-pool members. Routing set
		// preferred so cold-start named-pool traffic still respects the
		// K-gate, not just NodePattern. Intersect with present outbounds
		// so --validate (RenderOnly with nil outbounds) doesn't reference
		// proxies that aren't emitted.
		members = r.intersectWithPresent(r.routingMembers, outbounds)
		if len(members) == 0 {
			members = r.usPoolMembers(outbounds)
		}
	}
	if len(members) == 0 {
		return nil
	}
	// Fallback target is the pool name (not us-pool) so unmatched sources
	// still route through the pool's group rather than the default pool.
	return buildSliceRules(r.node.ClientSubnet, members, poolName)
}

// buildSliceRules generates the per-/32 SRC-IP-CIDR rules and a MATCH
// fallback for one perterm sub-rule.
func buildSliceRules(subnet string, members []string, fallbackProxy string) []string {
	hosts := enumerateHosts(subnet, maxPerTerminalHosts)
	if len(hosts) == 0 {
		return nil
	}
	rules := make([]string, 0, len(hosts)+1)
	for _, ip := range hosts {
		node := hrwPick(ip, members)
		rules = append(rules, fmt.Sprintf("SRC-IP-CIDR,%s/32,%s", ip, node))
	}
	rules = append(rules, "MATCH,"+fallbackProxy)
	return rules
}

// enumerateHosts returns the usable host IPs of a CIDR (excludes network +
// broadcast for IPv4 prefixes shorter than /31). Returns nil for a non-CIDR
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
		// Skip network (.0) and broadcast (last) for /30 and shorter.
		if hostBits >= 2 && (i == 0 || i == count-1) {
			continue
		}
		if len(out) >= limit {
			return nil // too many hosts — caller falls back to no slicing
		}
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], base+uint32(i))
		out = append(out, net.IP(b[:]).String())
	}
	return out
}

// hrwPick returns the member with the highest rendezvous hash for ip.
// Deterministic; removing a member only moves the IPs that hashed highest
// to it, leaving every other IP's assignment unchanged.
func hrwPick(ip string, members []string) string {
	var best string
	var bestScore uint64
	for _, m := range members {
		h := fnv.New64a()
		_, _ = h.Write([]byte(ip))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(m))
		s := h.Sum64()
		if best == "" || s > bestScore {
			best, bestScore = m, s
		}
	}
	return best
}
