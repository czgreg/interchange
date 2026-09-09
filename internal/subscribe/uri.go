package subscribe

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

func parseURIList(body []byte) ([]Outbound, error) {
	text := strings.TrimSpace(string(body))
	if d, ok := tryBase64(text); ok {
		text = d
	}
	var out []Outbound
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		o, err := parseURI(line)
		if err != nil {
			continue
		}
		if o.Tag() == "" {
			o["tag"] = fmt.Sprintf("uri-%d", i)
		}
		out = append(out, o)
	}
	return out, nil
}

func parseURI(s string) (Outbound, error) {
	switch {
	case strings.HasPrefix(s, "vmess://"):
		return parseVMessURI(s)
	case strings.HasPrefix(s, "vless://"):
		return parseGenericURI(s, "vless")
	case strings.HasPrefix(s, "trojan://"):
		return parseGenericURI(s, "trojan")
	case strings.HasPrefix(s, "ss://"):
		return parseSSURI(s)
	case strings.HasPrefix(s, "hysteria2://"), strings.HasPrefix(s, "hy2://"):
		return parseGenericURI(s, "hysteria2")
	case strings.HasPrefix(s, "tuic://"):
		return parseGenericURI(s, "tuic")
	}
	return nil, fmt.Errorf("unsupported uri scheme")
}

// vmess:// is base64-of-JSON in the v2rayN convention.
func parseVMessURI(s string) (Outbound, error) {
	b64 := strings.TrimPrefix(s, "vmess://")
	for _, dec := range []*base64.Encoding{
		base64.StdEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.RawURLEncoding,
	} {
		raw, err := dec.DecodeString(b64)
		if err != nil {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal(raw, &v); err != nil {
			continue
		}
		return vmessJSONToOutbound(v)
	}
	return nil, fmt.Errorf("vmess: bad base64/json")
}

func vmessJSONToOutbound(v map[string]any) (Outbound, error) {
	host, _ := v["add"].(string)
	port := getInt(v, "port")
	if host == "" || port == 0 {
		return nil, fmt.Errorf("vmess: missing add/port")
	}
	ps, _ := v["ps"].(string)
	o := Outbound{
		"type":        "vmess",
		"tag":         ps,
		"server":      host,
		"server_port": port,
		"uuid":        getString(v, "id"),
		"alter_id":    getInt(v, "aid"),
	}
	if scy, _ := v["scy"].(string); scy != "" {
		o["security"] = scy
	}
	if tlsStr, _ := v["tls"].(string); tlsStr == "tls" || tlsStr == "reality" {
		tls := map[string]any{"enabled": true}
		if sni := getString(v, "sni"); sni != "" {
			tls["server_name"] = sni
		} else if h := getString(v, "host"); h != "" {
			tls["server_name"] = h
		}
		o["tls"] = tls
	}
	net, _ := v["net"].(string)
	if net != "" && net != "tcp" {
		// sing-box transport types differ from the v2rayN convention:
		// - vmess JSON's "h2" → sing-box "http"
		// - grpc takes service_name, NOT path (vmess JSON overloads path for it)
		// - ws / http take path + Host header
		// Anything else we don't recognize → drop the transport rather than emit
		// a config sing-box will refuse at startup.
		sbType := net
		if net == "h2" {
			sbType = "http"
		}
		tr := map[string]any{"type": sbType}
		path := getString(v, "path")
		host := getString(v, "host")
		switch sbType {
		case "ws", "http":
			if path != "" {
				tr["path"] = path
			}
			if host != "" {
				tr["headers"] = map[string]any{"Host": host}
			}
		case "grpc":
			if path != "" {
				tr["service_name"] = path
			}
		default:
			tr = nil
		}
		if tr != nil {
			o["transport"] = tr
		}
	}
	return o, nil
}

// parseGenericURI handles vless / trojan / hysteria2 / tuic — they share
// the userinfo@host:port?query#fragment shape.
func parseGenericURI(raw, kind string) (Outbound, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	port, _ := strconv.Atoi(u.Port())
	if host == "" || port == 0 {
		return nil, fmt.Errorf("%s: missing host/port", kind)
	}
	q := u.Query()
	o := Outbound{
		"type":        kind,
		"tag":         u.Fragment,
		"server":      host,
		"server_port": port,
	}
	switch kind {
	case "vless":
		o["uuid"] = u.User.Username()
		if flow := q.Get("flow"); flow != "" {
			o["flow"] = flow
		}
	case "trojan", "hysteria2":
		if pwd, ok := u.User.Password(); ok && pwd != "" {
			o["password"] = pwd
		} else {
			o["password"] = u.User.Username()
		}
		// hysteria2 URIs carry the salamander obfuscation layer as
		// ?obfs=salamander&obfs-password=X. Same nested shape as the
		// clash and sing-box paths so all three round-trip identically
		// through internal/mihomo.applyObfsToClash.
		if kind == "hysteria2" {
			if ot := q.Get("obfs"); ot != "" {
				obfs := map[string]any{"type": ot}
				if pw := q.Get("obfs-password"); pw != "" {
					obfs["password"] = pw
				}
				o["obfs"] = obfs
			}
		}
	case "tuic":
		o["uuid"] = u.User.Username()
		if pwd, ok := u.User.Password(); ok {
			o["password"] = pwd
		}
		if cc := q.Get("congestion_control"); cc != "" {
			o["congestion_control"] = cc
		}
	}
	applyURIQueryTLS(o, kind, q)
	applyURIQueryTransport(o, q)
	return o, nil
}

func applyURIQueryTLS(o Outbound, kind string, q url.Values) {
	sec := q.Get("security")
	needTLS := kind == "hysteria2" || kind == "tuic" || sec == "tls" || sec == "reality"
	if !needTLS {
		return
	}
	tls := map[string]any{"enabled": true}
	if sni := q.Get("sni"); sni != "" {
		tls["server_name"] = sni
	} else if h := q.Get("peer"); h != "" {
		tls["server_name"] = h
	}
	if q.Get("allowInsecure") == "1" || q.Get("insecure") == "1" {
		tls["insecure"] = true
	}
	if alpn := q.Get("alpn"); alpn != "" {
		tls["alpn"] = strings.Split(alpn, ",")
	}
	if sec == "reality" {
		r := map[string]any{"enabled": true}
		if pbk := q.Get("pbk"); pbk != "" {
			r["public_key"] = pbk
		}
		if sid := q.Get("sid"); sid != "" {
			r["short_id"] = sid
		}
		tls["reality"] = r
	}
	// fp= is the uTLS browser fingerprint (e.g. "chrome"). Stored as a
	// sibling key on the outbound, not inside tls, matching how clash.go
	// and proxies.go handle it.
	if fp := q.Get("fp"); fp != "" {
		o["client-fingerprint"] = fp
	}
	o["tls"] = tls
}

func applyURIQueryTransport(o Outbound, q url.Values) {
	t := q.Get("type")
	if t == "" || t == "tcp" {
		return
	}
	sbType := t
	if t == "h2" {
		sbType = "http"
	}
	tr := map[string]any{"type": sbType}
	switch sbType {
	case "ws", "http":
		if p := q.Get("path"); p != "" {
			tr["path"] = p
		}
		if h := q.Get("host"); h != "" {
			tr["headers"] = map[string]any{"Host": h}
		}
	case "grpc":
		if n := q.Get("serviceName"); n != "" {
			tr["service_name"] = n
		}
	default:
		return
	}
	o["transport"] = tr
}

// SS URI has two flavors:
//
//	ss://BASE64(method:password)@host:port#tag           (legacy)
//	ss://BASE64(method:password@host:port)#tag           (older)
//	ss://method:password@host:port?plugin=...#tag        (SIP002 plain)
func parseSSURI(s string) (Outbound, error) {
	rest := strings.TrimPrefix(s, "ss://")
	tag := ""
	if i := strings.LastIndex(rest, "#"); i >= 0 {
		tag, _ = url.QueryUnescape(rest[i+1:])
		rest = rest[:i]
	}
	var query string
	if i := strings.LastIndex(rest, "?"); i >= 0 {
		query = rest[i+1:]
		rest = rest[:i]
	}
	var userinfo, hostport string
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		userinfo = rest[:at]
		hostport = rest[at+1:]
		if d, err := decodeAnyBase64(userinfo); err == nil {
			userinfo = d
		}
	} else {
		// older form: whole thing base64-encoded
		decoded, err := decodeAnyBase64(rest)
		if err != nil {
			return nil, fmt.Errorf("ss: bad base64")
		}
		at2 := strings.LastIndex(decoded, "@")
		if at2 < 0 {
			return nil, fmt.Errorf("ss: bad uri")
		}
		userinfo = decoded[:at2]
		hostport = decoded[at2+1:]
	}
	colon := strings.Index(userinfo, ":")
	if colon < 0 {
		return nil, fmt.Errorf("ss: bad userinfo")
	}
	method := userinfo[:colon]
	password := userinfo[colon+1:]
	hostStr, portStr, found := strings.Cut(hostport, ":")
	if !found {
		return nil, fmt.Errorf("ss: missing port")
	}
	port, _ := strconv.Atoi(portStr)
	if port == 0 {
		return nil, fmt.Errorf("ss: bad port")
	}
	o := Outbound{
		"type":        "shadowsocks",
		"tag":         tag,
		"server":      hostStr,
		"server_port": port,
		"method":      method,
		"password":    password,
	}
	if query != "" {
		q, _ := url.ParseQuery(query)
		if p := q.Get("plugin"); p != "" {
			o["plugin"] = p
		}
	}
	return o, nil
}

func decodeAnyBase64(s string) (string, error) {
	for _, dec := range []*base64.Encoding{
		base64.StdEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.RawURLEncoding,
	} {
		if b, err := dec.DecodeString(s); err == nil {
			return string(b), nil
		}
	}
	return "", fmt.Errorf("not base64")
}

func getString(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}
