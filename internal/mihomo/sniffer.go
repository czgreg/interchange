// sniffer.go — protocol sniffer (TLS SNI / HTTP Host / QUIC ALPN extraction).
//
// Why this is load-bearing:
//   Under redir-host DNS there is no fake-ip back-mapping — mihomo's TUN
//   sees the real destination IP and nothing else. The sniffer is the ONLY
//   mechanism that recovers the domain: it reads the SNI / Host / ALPN off
//   the connection itself and rewrites the destination from IP → domain, so
//   geosite rules can classify the flow. Without it, only geoip rules could
//   match and every domain-keyed route would miss.
//
//   This matters most for client-side DoH/DoT (Chrome's "Use Secure DNS"):
//   the browser resolves gemini.google.com itself and opens a TLS
//   connection straight to the real IP, never touching mihomo's DNS. The
//   sniffer still extracts the SNI and routing works regardless.
//
//   It also stabilizes egress: us-pool's consistent-hashing keys by
//   destination. Keyed by raw IP, a single Google session's many parallel
//   flows hit many Google IPs and spread across many proxy nodes — Google
//   sees one session from multiple ASNs and trips its session-anomaly
//   response. Keyed by domain (post-sniff), every flow of one site sticks
//   to one node: a single egress IP per session.
//
// Production trigger: 2026-06-06, post mihomo cutover on 89.
//
// Tunables explained:
//   - parse-pure-ip:        sniff even when the destination is a pure IP.
//                           Under redir-host every proxied flow is exactly
//                           this case (real IP, no fake-ip mapping), so it
//                           must be on.
//   - force-dns-mapping:    also sniff connections that arrived with a
//                           DNS→IP mapping. Belt-and-suspenders.
//   - override-destination: rewrite dst from IP to domain so subsequent
//                           rule matching sees the domain.

package mihomo

func (r *Renderer) buildSniffer() map[string]any {
	return map[string]any{
		"enable":               true,
		"force-dns-mapping":    true,
		"parse-pure-ip":        true,
		"override-destination": true,
		"sniff": map[string]any{
			"TLS":  map[string]any{"ports": []any{"443", "8443"}},
			"HTTP": map[string]any{"ports": []any{"80", "8080-8880"}},
			"QUIC": map[string]any{"ports": []any{"443"}},
		},
	}
}
