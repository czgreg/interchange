// dns.go — mihomo DNS block.
//
// enhanced-mode: redir-host (real IPs, no fake-ip).
//
//   Every domain resolves to its real address. Routing does NOT depend
//   on DNS: mihomo's sniffer (see sniffer.go) extracts the domain from
//   TLS SNI / HTTP Host / QUIC ALPN — including from pure-IP flows via
//   parse-pure-ip — and geosite rule-sets classify from that. The DNS
//   answer's IP is never the routing key, so returning the real IP costs
//   nothing in routing correctness.
//
//   We ran fake-ip until 2026-07-06. It was a global default-hijack model
//   (every domain gets a 198.18.x.x unless carved out in fake-ip-filter)
//   and its two theoretical wins don't apply here:
//     - consistent-hash key stability under load-balance — N/A, we route
//       per-terminal (single node per SRC-IP), not by dst-IP hash.
//     - DNS-poison defense — already covered: proxied domains resolve via
//       proxyDoH through the overseas pool, never the pollutable CN path;
//       and TPROXY forces all client DNS through us, so the sniffer path
//       (not fake-ip back-mapping) is what actually carries routing.
//   Its recurring cost was real: any hostname not in fake-ip-filter got a
//   198.18.x.x and then timed out on DIRECT/probe dial. Bit intranet
//   services (ai.paigod.work, 2026-06-05) and, when a subscription
//   provider's node domain was missed, silently killed node health probes
//   — every node alive=False because mihomo dialed the fake IP
//   (11us.quandao.com, 2026-07-06). redir-host removes that failure mode
//   entirely: node server domains and intranet hosts resolve to real IPs
//   with zero per-provider maintenance.
//
// Schema:
//   - default-nameserver: bare-IP UDP, used to bootstrap-resolve the DoH
//     hostnames in nameserver / nameserver-policy themselves.
//   - nameserver:        proxy DoH — default for everything not matched
//                        by nameserver-policy. Routed through us-pool by
//                        mihomo's outbound dispatch. Overseas domains
//                        resolve through the proxy, dodging CN poisoning.
//   - nameserver-policy: domain-keyed routing. CN domains and intranet
//                        skip-suffixes resolve direct via CN DoH; this
//                        short-circuits the parallel-fallback pattern
//                        (see "Why no fallback-filter" below).
//   - proxy-server-nameserver: airport-node hostname resolution. CN DoH
//                        plus cfg.DNS.NodeResolver (default plain UDP to
//                        119.29.29.29). Never proxyDoH — that would recurse
//                        through the proxy we're trying to set up. Plain
//                        UDP is safe here for the same reason
//                        default-nameserver is: it's an IP literal, so it
//                        needs no prior resolution. The UDP upstream is
//                        required because a DoH frontend answering NXDOMAIN
//                        for a live node hostname is not recoverable within
//                        psn — see buildProxyServerNameserver.
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
// FakeIPSkipSuffixes (cfg.DNS) is retained but now feeds ONLY
// nameserver-policy: intranet suffixes (paigod.work, feilian.cn, ...)
// must resolve via CN DoH, because the default nameserver is proxyDoH
// (overseas) which has no records for intranet domains. Under fake-ip
// these suffixes also had to be listed in fake-ip-filter; under
// redir-host that carve-out is unnecessary — nothing is hijacked.

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
	nodeResolver := r.cfg.DNS.NodeResolver
	if len(nodeResolver) == 0 {
		nodeResolver = []string{"udp://119.29.29.29"}
	}

	dns := map[string]any{
		"enable":                  true,
		"ipv6":                    false,
		"enhanced-mode":           "redir-host",
		"default-nameserver":      []string{stripScheme(bootstrap)},
		"nameserver":              proxyDoH,
		"nameserver-policy":       buildNameserverPolicy(cnDoH, r.cfg.DNS.FakeIPSkipSuffixes),
		"proxy-server-nameserver": buildProxyServerNameserver(cnDoH, nodeResolver),
	}

	if l := r.cfg.DNS.Listen; l != "" {
		dns["listen"] = l
	} else if r.node.Tun0GatewayIP != "" {
		dns["listen"] = r.node.Tun0GatewayIP + ":53"
	}

	return dns
}

// buildProxyServerNameserver returns the upstream list for mihomo's
// proxy-server-nameserver: CN DoH first (unchanged behavior, keeps node
// lookups off the proxy we're trying to build), then the node-resolver
// upstreams appended and deduplicated.
//
// The appended plain-UDP upstream is not redundant with CN DoH. psn is a
// distinct resolver instance from `nameserver`, and a DoH frontend that
// answers NXDOMAIN for a live name yields "dns resolve failed: couldn't
// find ip" at dial time. Measured on node 92 (2026-08-12) against
// z7mp4a9xq2k8f3r-r9.edg3.org over 15 dials: [doh.pub, alidns] produced 22
// resolve failures; the same list plus udp://119.29.29.29 produced 1.
// See DNSConfig.NodeResolver for why this is separate from CNDoH.
func buildProxyServerNameserver(cnDoH, nodeResolver []string) []string {
	out := make([]string, 0, len(cnDoH)+len(nodeResolver))
	seen := make(map[string]bool, len(cnDoH)+len(nodeResolver))
	for _, u := range cnDoH {
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	for _, u := range nodeResolver {
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
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
// "+.paigod.work" and returns "+.paigod.work" — the wildcard form
// mihomo's nameserver-policy keys expect.
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
