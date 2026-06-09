// proxies.go — sing-box Outbound → mihomo Clash proxy entry.
//
// This is the inverse of internal/subscribe/clash.go's clashProxyToOutbound,
// with one critical reverse: when we encoded a clash node into a sing-box
// outbound (by parser path), we lifted client-fingerprint into tls.utls.fingerprint.
// Going back to clash, we drop tls.utls and re-emit client-fingerprint as a
// sibling key (mihomo expects this layout).
//
// Coverage matches what the subscribe package actually parses:
//   shadowsocks, trojan, vmess, vless, hysteria2, tuic.
// Anything else is dropped (filtered by caller — never reaches a mihomo
// outbound entry).

package mihomo

import (
	"fmt"

	"github.com/leap-gateway/leap-gateway/internal/subscribe"
)

// outboundToProxy converts a single sing-box outbound (the internal Outbound
// shape used everywhere in leap-gateway) to a mihomo Clash proxy entry.
// Returns nil if the type is not a node-bearing outbound (selector, urltest,
// direct, block, dns, etc. — these don't translate, mihomo synthesizes its
// own infrastructure outbounds from proxy-groups).
func outboundToProxy(o subscribe.Outbound) map[string]any {
	t := o.Type()
	switch t {
	case "shadowsocks", "trojan", "vmess", "vless", "hysteria2", "tuic":
		// fall through
	default:
		return nil
	}

	p := map[string]any{
		"name":   o.Tag(),
		"server": o["server"],
		"port":   o["server_port"],
		"udp":    true,
	}

	switch t {
	case "shadowsocks":
		p["type"] = "ss"
		if v, ok := o["method"].(string); ok && v != "" {
			p["cipher"] = v
		}
		if v, ok := o["password"].(string); ok && v != "" {
			p["password"] = v
		}
	case "trojan":
		p["type"] = "trojan"
		if v, ok := o["password"].(string); ok && v != "" {
			p["password"] = v
		}
		applyTLSToClash(p, o)
	case "vmess":
		p["type"] = "vmess"
		if v, ok := o["uuid"].(string); ok && v != "" {
			p["uuid"] = v
		}
		// alter_id may be int or float64 depending on origin parser path.
		switch v := o["alter_id"].(type) {
		case int:
			p["alterId"] = v
		case float64:
			p["alterId"] = int(v)
		}
		if v, ok := o["security"].(string); ok && v != "" {
			p["cipher"] = v
		}
		applyTLSToClash(p, o)
		applyTransportToClash(p, o)
	case "vless":
		p["type"] = "vless"
		if v, ok := o["uuid"].(string); ok && v != "" {
			p["uuid"] = v
		}
		if v, ok := o["flow"].(string); ok && v != "" {
			p["flow"] = v
		}
		// packet_encoding (xudp) is sing-box specific; mihomo uses
		// `packet-encoding` at proxy level. Honor it if present.
		if v, ok := o["packet_encoding"].(string); ok && v != "" {
			p["packet-encoding"] = v
		}
		applyTLSToClash(p, o)
		applyTransportToClash(p, o)
	case "hysteria2":
		p["type"] = "hysteria2"
		if v, ok := o["password"].(string); ok && v != "" {
			p["password"] = v
		}
		applyTLSToClash(p, o)
	case "tuic":
		p["type"] = "tuic"
		if v, ok := o["uuid"].(string); ok && v != "" {
			p["uuid"] = v
		}
		if v, ok := o["password"].(string); ok && v != "" {
			p["password"] = v
		}
		applyTLSToClash(p, o)
	}

	return p
}

// applyTLSToClash maps a sing-box tls block back to clash sibling keys.
// Reverse of internal/subscribe/clash.go::applyClashTLS.
func applyTLSToClash(p map[string]any, o subscribe.Outbound) {
	tls, ok := o["tls"].(map[string]any)
	if !ok {
		return
	}
	enabled, _ := tls["enabled"].(bool)
	// hysteria2/tuic always TLS — clash assumes it; don't emit `tls: true`
	// for those (some clients don't even accept it). For trojan/vmess/vless
	// emit explicitly when enabled.
	t, _ := p["type"].(string)
	if enabled && t != "hysteria2" && t != "tuic" {
		p["tls"] = true
	}
	if v, ok := tls["server_name"].(string); ok && v != "" {
		// vless uses `servername`, vmess/trojan use `sni`. Clash mihomo
		// accepts both, but the canonical for vless is `servername`.
		// We pick the one appropriate to type.
		switch t {
		case "vless":
			p["servername"] = v
		default:
			p["sni"] = v
		}
	}
	if v, ok := tls["insecure"].(bool); ok && v {
		p["skip-cert-verify"] = true
	}
	if v, ok := tls["alpn"].([]any); ok && len(v) > 0 {
		p["alpn"] = v
	} else if v, ok := tls["alpn"].([]string); ok && len(v) > 0 {
		p["alpn"] = v
	}
	if reality, ok := tls["reality"].(map[string]any); ok {
		ro := map[string]any{}
		if v, ok := reality["public_key"].(string); ok && v != "" {
			ro["public-key"] = v
		}
		if v, ok := reality["short_id"].(string); ok && v != "" {
			ro["short-id"] = v
		}
		p["reality-opts"] = ro
	}
	// Surface the utls fingerprint as the sibling client-fingerprint key.
	// We synthesize it on the parse side (clash.go) when reality is on; if
	// it leaked through some other path with utls but no reality, still
	// preserve the user's intent.
	if utls, ok := tls["utls"].(map[string]any); ok {
		if fp, ok := utls["fingerprint"].(string); ok && fp != "" {
			p["client-fingerprint"] = fp
		}
	}
}

// applyTransportToClash maps a sing-box transport block back to clash's
// network: + ws-opts: / grpc-opts: layout.
func applyTransportToClash(p map[string]any, o subscribe.Outbound) {
	tr, ok := o["transport"].(map[string]any)
	if !ok {
		return
	}
	switch tr["type"] {
	case "ws":
		p["network"] = "ws"
		opts := map[string]any{}
		if v, ok := tr["path"].(string); ok && v != "" {
			opts["path"] = v
		}
		if h, ok := tr["headers"].(map[string]any); ok && len(h) > 0 {
			opts["headers"] = normalizeWSHeaders(h)
		}
		if len(opts) > 0 {
			p["ws-opts"] = opts
		}
	case "grpc":
		p["network"] = "grpc"
		opts := map[string]any{}
		if v, ok := tr["service_name"].(string); ok && v != "" {
			opts["grpc-service-name"] = v
		}
		if len(opts) > 0 {
			p["grpc-opts"] = opts
		}
	case "http":
		p["network"] = "h2"
	}
}

// normalizeWSHeaders coerces ws-opts.headers values to strings. mihomo's
// schema requires `map[string]string`; some upstream clash subscriptions
// (cyberguard, observed 2026-06-09 on 89) emit Host as a YAML list —
// passing that through verbatim makes mihomo refuse the config at startup
// with `'ws-opts.headers[Host]' expected type 'string', got
// unconvertible type '[]interface {}'`. Take the first element of a list,
// fmt.Sprint anything else, drop nil/empty.
func normalizeWSHeaders(h map[string]any) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		switch x := v.(type) {
		case nil:
			continue
		case string:
			if x != "" {
				out[k] = x
			}
		case []any:
			if len(x) == 0 {
				continue
			}
			s := fmt.Sprint(x[0])
			if s != "" {
				out[k] = s
			}
		case []string:
			if len(x) == 0 {
				continue
			}
			if x[0] != "" {
				out[k] = x[0]
			}
		default:
			s := fmt.Sprint(x)
			if s != "" {
				out[k] = s
			}
		}
	}
	return out
}
