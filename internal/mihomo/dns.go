// dns.go — mihomo DNS block.
//
// Mirrors the sing-box renderer's DNS semantics (docs/dns.md §6) but in
// mihomo's flat schema.
//
//   - fakeip enabled, range = cfg.DNS.FakeIPRange (default 198.18.0.0/15).
//   - nameserver: cn-doh entries (resolved via direct).
//   - fallback: proxy-doh entries (resolved through us-pool).
//   - fallback-filter: GEOIP CN → use nameserver, else fallback. Mirrors
//     sing-box's "CN domain → local, else remote".
//   - fake-ip-filter: domains that must NEVER receive a fake IP. Defaults
//     cover .lan / .local / .cn / qq edge. Operators extend this with
//     cfg.DNS.FakeIPSkipSuffixes for internal services whose hostnames
//     are not on geosite-cn but resolve to CN/LAN IPs that need DIRECT
//     reachability (paigod.work, feilian.cn, etc.). Without those skip
//     entries, mihomo's fakeip mode hands out 198.18.x.x for those hosts
//     and the DIRECT outbound dials the unreachable fake IP — caught in
//     production 2026-06-05 (ai.paigod.work timeouts).
//
// fakeip is enhanced-mode and applies GLOBALLY: any domain not in
// fake-ip-filter receives a fake IP, regardless of nameserver-policy.
// To carve out a domain you MUST list it in fake-ip-filter.

package mihomo

func (r *Renderer) buildDNS() map[string]any {
	cnDoH := r.cfg.DNS.CNDoH
	if len(cnDoH) == 0 {
		cnDoH = []string{"https://doh.pub/dns-query"}
	}
	proxyDoH := r.cfg.DNS.ProxyDoH
	if len(proxyDoH) == 0 {
		proxyDoH = []string{"tls://1.1.1.1:853"}
	}
	bootstrap := r.cfg.DNS.BootstrapResolver
	if bootstrap == "" {
		bootstrap = "udp://119.29.29.29"
	}

	dns := map[string]any{
		"enable":             true,
		"ipv6":               false,
		"enhanced-mode":      "fake-ip",
		"fake-ip-range":      defaultIfEmpty(r.cfg.DNS.FakeIPRange, "198.18.0.0/15"),
		"default-nameserver": []string{stripScheme(bootstrap)},
		"nameserver":         cnDoH,
		"fallback":           proxyDoH,
		"fallback-filter": map[string]any{
			"geoip":      true,
			"geoip-code": "CN",
		},
		// Don't fake-ip CN domains — they go direct and need real IPs.
		// Operators can extend this list via cfg.DNS.FakeIPSkipSuffixes
		// for internal/private hostnames not on geosite-cn (paigod.work,
		// feilian.cn, intranet *.work etc.).
		"fake-ip-filter": fakeIPFilter(r.cfg.DNS.FakeIPSkipSuffixes),
		"proxy-server-nameserver": cnDoH,
	}

	// Bind the DNS server on the FeiLian client-facing tun0 gateway IP
	// (clients have it pushed as their DNS via FeiLian SaaS) so their
	// queries hit mihomo and get fakeip back. sing-box did this with a
	// "direct" inbound on tun0_gateway_ip:53; mihomo's equivalent is the
	// dns.listen field. WITHOUT this, employee browsers see DNS timeout
	// → can't reach overseas — caught in production 2026-06-05.
	//
	// DNS.Listen overrides if set; default = <Tun0GatewayIP>:53. Empty
	// node IP (lab/selftest) → omit listen, mihomo binds nowhere.
	if l := r.cfg.DNS.Listen; l != "" {
		dns["listen"] = l
	} else if r.node.Tun0GatewayIP != "" {
		dns["listen"] = r.node.Tun0GatewayIP + ":53"
	}

	return dns
}

// stripScheme normalizes "udp://119.29.29.29" → "119.29.29.29" because
// mihomo's default-nameserver expects bare IPs (it builds its own UDP
// resolver on port 53).
func stripScheme(s string) string {
	for _, p := range []string{"udp://", "tcp://", "tls://", "https://", "h3://"} {
		if len(s) > len(p) && s[:len(p)] == p {
			return s[len(p):]
		}
	}
	return s
}

// fakeIPFilter returns the merged "+.<suffix>" list emitted into
// dns.fake-ip-filter. Defaults (lan/local/cn + qq edge) are always
// included; operator-supplied extras are normalized to "+." prefix and
// appended in stable order. Duplicate suffixes are collapsed.
func fakeIPFilter(extra []string) []string {
	defaults := []string{
		"+.lan",
		"+.local",
		"+.cn",
		"localhost.ptlogin2.qq.com",
	}
	seen := make(map[string]bool, len(defaults)+len(extra))
	out := make([]string, 0, len(defaults)+len(extra))
	for _, d := range defaults {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for _, raw := range extra {
		s := raw
		// Accept the user typing ".paigod.work", "paigod.work", or
		// "+.paigod.work". Always emit the "+." prefix form.
		if len(s) == 0 {
			continue
		}
		if s[0] == '.' {
			s = "+" + s
		} else if len(s) < 2 || s[:2] != "+." {
			s = "+." + s
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
