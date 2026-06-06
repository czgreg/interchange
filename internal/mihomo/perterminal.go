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

// perTerminalSlices returns the `perterm` sub-rule body: one
// SRC-IP-CIDR,<host>/32,<node> per client terminal IP (HRW-assigned across
// the current us-pool members) followed by a MATCH,us-pool fallback.
// Returns nil if there are no members or the subnet can't be sliced.
func (r *Renderer) perTerminalSlices(outbounds []subscribe.Outbound) []string {
	members := r.usPoolMembers(outbounds)
	if len(members) == 0 {
		return nil
	}
	hosts := enumerateHosts(r.node.ClientSubnet, maxPerTerminalHosts)
	if len(hosts) == 0 {
		return nil
	}
	rules := make([]string, 0, len(hosts)+1)
	for _, ip := range hosts {
		node := hrwPick(ip, members)
		rules = append(rules, fmt.Sprintf("SRC-IP-CIDR,%s/32,%s", ip, node))
	}
	// Fallback: any source not in the enumerated set still egresses (the
	// load-balance pool, per-destination). Keeps the rule total bounded and
	// covers off-subnet sources gracefully.
	rules = append(rules, "MATCH,"+usPool)
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
