// sniffer.go — protocol sniffer (TLS SNI / HTTP Host / QUIC ALPN extraction).
//
// Mirrors internal/singbox/renderer.go's TUN inbound
// `sniff: true, sniff_override_destination: true`.
//
// Why this is load-bearing:
//   - Chrome's "Use Secure DNS" (and any client-side DoH/DoT) bypasses
//     mihomo's DNS server entirely. The browser hands itself a REAL IP
//     for gemini.google.com, then opens a TLS connection to that IP.
//     mihomo's TUN sees only the raw IP — fake-ip back-mapping doesn't
//     fire (no fake-ip was ever issued), so without sniffing, only
//     geoip rules can classify the flow.
//   - Worse, us-pool's consistent-hashing strategy hashes by destination
//     IP. A single Google session has many parallel TLS flows to many
//     Google IPs, which spread across many proxy nodes — Google sees
//     one logged-in session arriving from multiple ASNs simultaneously
//     and trips its session-anomaly response (the "Use secure DNS"
//     diagnostic users see).
//
// With sniffer on:
//   - Every TLS / HTTP / QUIC flow has its SNI / Host / ALPN extracted
//     and the destination is rewritten from IP → domain.
//   - geosite rules now apply (RULE-SET,geosite-google,DIRECT etc.).
//   - load-balance hashing keys by domain — every flow of one site
//     sticks to one node, so Google sees a single egress IP per session.
//
// Production trigger: 2026-06-06, post mihomo cutover on 89. sing-box
// had the equivalent setting from day one; the mihomo renderer didn't.
//
// Tunables explained:
//   - parse-pure-ip:        sniff even when destination is a pure IP
//                           (no prior fake-ip mapping). Default is
//                           protocol-version-dependent; we set it
//                           explicitly because the DoH case is exactly
//                           "pure IP, no fake-ip".
//   - force-dns-mapping:    also sniff connections that DO have a
//                           fake-ip mapping. Belt-and-suspenders.
//   - override-destination: rewrite dst from IP to domain so
//                           subsequent rule matching sees the domain.

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
