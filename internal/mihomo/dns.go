// dns.go — mihomo DNS block.
//
// Schema:
//   - enhanced-mode: fake-ip (global; carve-outs via fake-ip-filter).
//   - default-nameserver: bare-IP UDP, used to bootstrap-resolve the DoH
//     hostnames in nameserver / nameserver-policy themselves.
//   - nameserver:        proxy DoH — default for everything not matched
//                        by nameserver-policy. Routed through us-pool by
//                        mihomo's outbound dispatch.
//   - nameserver-policy: domain-keyed routing. CN domains and intranet
//                        skip-suffixes resolve direct via CN DoH; this
//                        short-circuits the parallel-fallback pattern
//                        (see "Why no fallback-filter" below).
//   - proxy-server-nameserver: airport-node hostname resolution. Always
//                        CN DoH so we don't recurse through the proxy
//                        we're trying to set up.
//   - fake-ip-filter:    domains that must NEVER receive a fake IP.
//                        Defaults cover .lan / .local / .cn / qq edge.
//                        Operators extend via cfg.DNS.FakeIPSkipSuffixes
//                        for internal services whose hostnames aren't on
//                        geosite-cn but resolve to CN/LAN IPs that need
//                        DIRECT reachability (paigod.work, feilian.cn,
//                        etc.). Without those skip entries, mihomo's
//                        fakeip mode hands out 198.18.x.x for them and
//                        DIRECT dials the unreachable fake IP — caught
//                        in production 2026-06-05 (ai.paigod.work).
//
// Why no fallback-filter:
//   The earlier setup used `nameserver: cnDoH; fallback: proxyDoH;
//   fallback-filter: geoip CN` — sound but wasteful: every DNS query
//   fired BOTH servers in parallel and picked by response-IP geo. With
//   nameserver-policy keyed off rule-set:geosite-cn, every CN query
//   short-circuits to one direct lookup; everything else is one query
//   through proxy. No parallel fan-out. ~50% DNS-traffic reduction
//   under typical CN-heavy workload, plus deterministic single-RTT
//   resolution.
//
// Why fake-ip-skip suffixes also appear in nameserver-policy:
//   Skip-suffixes (paigod.work, feilian.cn, ...) MUST resolve via CN
//   DoH. They land in fake-ip-filter so mihomo doesn't fakeip them, but
//   the resolution itself flows through the nameserver chain — and the
//   default nameserver is proxyDoH (overseas), which won't have records
//   for intranet domains. Mirroring the suffix list into nameserver-
//   policy points each one explicitly at CN DoH.

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
		"enable":                  true,
		"ipv6":                    false,
		"enhanced-mode":           "fake-ip",
		"fake-ip-range":           defaultIfEmpty(r.cfg.DNS.FakeIPRange, "198.18.0.0/15"),
		"default-nameserver":      []string{stripScheme(bootstrap)},
		"nameserver":              proxyDoH,
		"nameserver-policy":       buildNameserverPolicy(cnDoH, r.cfg.DNS.FakeIPSkipSuffixes),
		"fake-ip-filter":          fakeIPFilter(r.cfg.DNS.FakeIPSkipSuffixes),
		"proxy-server-nameserver": cnDoH,
	}

	if l := r.cfg.DNS.Listen; l != "" {
		dns["listen"] = l
	} else if r.node.Tun0GatewayIP != "" {
		dns["listen"] = r.node.Tun0GatewayIP + ":53"
	}

	return dns
}

// buildNameserverPolicy maps domain-class keys to CN DoH so that:
//   - CN domains (rule-set:geosite-cn) resolve direct (no parallel proxy hit)
//   - operator-supplied fake-ip-skip suffixes (paigod.work, feilian.cn etc.)
//     resolve via CN DoH so DIRECT outbound gets a usable IP
//
// Keys mihomo accepts here: domain wildcards (`+.<suffix>`),
// `geosite:<class>`, and `rule-set:<provider-name>`. We use rule-set
// because the rule-provider geosite-cn is already declared in
// rule-providers (loaded from .mrs on disk) — no second copy of the
// domain database, no dependency on bundled geodata.
func buildNameserverPolicy(cnDoH, fakeIPSkip []string) map[string]any {
	policy := map[string]any{
		"rule-set:geosite-cn": cnDoH,
	}
	seen := map[string]bool{"rule-set:geosite-cn": true}
	for _, raw := range fakeIPSkip {
		s := normalizePolicySuffix(raw)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		policy[s] = cnDoH
	}
	return policy
}

// normalizePolicySuffix accepts ".paigod.work", "paigod.work", or
// "+.paigod.work" and returns "+.paigod.work". Same shape as
// fakeIPFilter's normalization so the two stay in lockstep.
func normalizePolicySuffix(s string) string {
	if len(s) == 0 {
		return ""
	}
	if s[0] == '.' {
		return "+" + s
	}
	if len(s) >= 2 && s[:2] == "+." {
		return s
	}
	return "+." + s
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
