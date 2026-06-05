package subscribe

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

type clashDoc struct {
	Proxies []map[string]any `yaml:"proxies"`
}

func parseClash(body []byte) ([]Outbound, error) {
	var doc clashDoc
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("clash yaml: %w", err)
	}
	var out []Outbound
	for i, p := range doc.Proxies {
		o, err := clashProxyToOutbound(p)
		if err != nil {
			// skip unsupported nodes; do not abort the entire subscription
			continue
		}
		if o.Tag() == "" {
			o["tag"] = fmt.Sprintf("clash-%d", i)
		}
		out = append(out, o)
	}
	return out, nil
}

func clashProxyToOutbound(p map[string]any) (Outbound, error) {
	typ, _ := p["type"].(string)
	name, _ := p["name"].(string)
	server, _ := p["server"].(string)
	port := getInt(p, "port")
	if server == "" || port == 0 {
		return nil, fmt.Errorf("missing server/port")
	}
	o := Outbound{
		"tag":         name,
		"server":      server,
		"server_port": port,
	}
	switch typ {
	case "ss":
		o["type"] = "shadowsocks"
		o["method"], _ = p["cipher"].(string)
		o["password"], _ = p["password"].(string)
	case "trojan":
		o["type"] = "trojan"
		o["password"], _ = p["password"].(string)
		applyClashTLS(o, p)
	case "vmess":
		o["type"] = "vmess"
		o["uuid"], _ = p["uuid"].(string)
		o["alter_id"] = getInt(p, "alterId")
		if c, ok := p["cipher"].(string); ok && c != "" {
			o["security"] = c
		}
		applyClashTLS(o, p)
		applyClashTransport(o, p)
	case "vless":
		o["type"] = "vless"
		o["uuid"], _ = p["uuid"].(string)
		if flow, ok := p["flow"].(string); ok && flow != "" {
			o["flow"] = flow
		}
		applyClashTLS(o, p)
		applyClashTransport(o, p)
	case "hysteria2":
		o["type"] = "hysteria2"
		o["password"], _ = p["password"].(string)
		applyClashTLS(o, p)
	case "tuic":
		o["type"] = "tuic"
		o["uuid"], _ = p["uuid"].(string)
		o["password"], _ = p["password"].(string)
		applyClashTLS(o, p)
	default:
		return nil, fmt.Errorf("unsupported clash type %q", typ)
	}
	return o, nil
}

func applyClashTLS(o Outbound, p map[string]any) {
	tlsOn, _ := p["tls"].(bool)
	sni, _ := p["sni"].(string)
	if !tlsOn && sni == "" {
		// hysteria2/tuic always TLS even without explicit flag
		if t, _ := o["type"].(string); t != "hysteria2" && t != "tuic" {
			return
		}
	}
	tls := map[string]any{"enabled": true}
	if sni != "" {
		tls["server_name"] = sni
	} else if h, ok := p["servername"].(string); ok && h != "" {
		tls["server_name"] = h
	}
	if v, ok := p["skip-cert-verify"].(bool); ok && v {
		tls["insecure"] = true
	}
	if alpn := getStringSlice(p, "alpn"); len(alpn) > 0 {
		tls["alpn"] = alpn
	}
	if reality, ok := p["reality-opts"].(map[string]any); ok {
		r := map[string]any{"enabled": true}
		if pk, ok := reality["public-key"].(string); ok {
			r["public_key"] = pk
		}
		if sid, ok := reality["short-id"].(string); ok {
			r["short_id"] = sid
		}
		tls["reality"] = r
	}
	// uTLS fingerprint. sing-box REQUIRES tls.utls when tls.reality is on
	// (FATAL "uTLS is required by reality client" otherwise). Clash YAML
	// carries this as a sibling key `client-fingerprint` of the proxy entry
	// rather than nested in reality-opts. Always emit utls when reality is
	// enabled, falling back to "chrome" if the airport didn't specify.
	fp, _ := p["client-fingerprint"].(string)
	if _, hasReality := tls["reality"]; hasReality {
		if fp == "" {
			fp = "chrome"
		}
		tls["utls"] = map[string]any{"enabled": true, "fingerprint": fp}
	} else if fp != "" {
		tls["utls"] = map[string]any{"enabled": true, "fingerprint": fp}
	}
	o["tls"] = tls
}

func applyClashTransport(o Outbound, p map[string]any) {
	net, _ := p["network"].(string)
	if net == "" || net == "tcp" {
		return
	}
	tr := map[string]any{}
	switch net {
	case "ws":
		tr["type"] = "ws"
		if opts, ok := p["ws-opts"].(map[string]any); ok {
			if path, ok := opts["path"].(string); ok {
				tr["path"] = path
			}
			if headers, ok := opts["headers"].(map[string]any); ok {
				tr["headers"] = headers
			}
		}
	case "grpc":
		tr["type"] = "grpc"
		if opts, ok := p["grpc-opts"].(map[string]any); ok {
			if name, ok := opts["grpc-service-name"].(string); ok {
				tr["service_name"] = name
			}
		}
	case "h2":
		tr["type"] = "http"
	default:
		return
	}
	o["transport"] = tr
}

func getInt(m map[string]any, k string) int {
	switch v := m[k].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case string:
		var i int
		fmt.Sscanf(v, "%d", &i)
		return i
	}
	return 0
}

func getStringSlice(m map[string]any, k string) []string {
	v, ok := m[k].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(v))
	for _, e := range v {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
