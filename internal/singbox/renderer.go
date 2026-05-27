package singbox

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"

	"github.com/leap-gateway/leap-gateway/internal/config"
	"github.com/leap-gateway/leap-gateway/internal/subscribe"
)

// Renderer turns a list of subscription outbounds + node config into a
// complete sing-box config.json. The output is suitable for `sing-box check`
// and for direct `sing-box run` after the node-side ip rule + nft injection.
type Renderer struct {
	cfg  config.SingBoxConfig
	node config.NodeConfig
}

func NewRenderer(cfg config.SingBoxConfig) *Renderer {
	return &Renderer{cfg: cfg}
}

// WithNode attaches NodeConfig (client_subnet / tun0_gateway_ip / egress_iface)
// so the renderer can emit the TUN inbound and the DNS-direct inbound bound to
// the FeiLian client gateway IP. If WithNode is not called or the gateway IP is
// empty, the renderer falls back to a "lab" mode with no TUN/DNS-direct
// inbounds — useful for cmd/selftest static validation.
func (r *Renderer) WithNode(n config.NodeConfig) *Renderer {
	r.node = n
	return r
}

// Path is the on-disk path of the rendered config.
func (r *Renderer) Path() string { return r.cfg.ConfigPath }

// Write renders a complete sing-box config from the given outbounds and
// atomically writes it to ConfigPath. Returns the rendered bytes.
func (r *Renderer) Write(outbounds []subscribe.Outbound) ([]byte, error) {
	cfg := r.build(outbounds)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(r.cfg.ConfigPath), 0o755); err != nil {
		return nil, err
	}
	tmp := r.cfg.ConfigPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, r.cfg.ConfigPath); err != nil {
		return nil, err
	}
	return data, nil
}

func (r *Renderer) build(outbounds []subscribe.Outbound) map[string]any {
	return map[string]any{
		"log":          r.buildLog(),
		"dns":          r.buildDNS(),
		"inbounds":     r.buildInbounds(),
		"outbounds":    r.buildOutbounds(outbounds),
		"route":        r.buildRoute(),
		"experimental": r.buildExperimental(),
	}
}

func (r *Renderer) buildLog() map[string]any {
	return map[string]any{
		"level":     r.cfg.LogLevel,
		"timestamp": true,
	}
}

// buildDNS emits the DNS section per docs/dns.md §6.
//
// servers:
//   - "remote" — first ProxyDoH entry, detour=out (goes through airport proxy)
//   - "local"  — first CNDoH entry,    detour=direct
//   - "fakeip" — sing-box internal fake-IP pool
//   - "local-bootstrap" — IP-literal UDP, used by remote/local to resolve their
//     own host names without chicken-and-egg
//
// rules: outbound=any → local (avoid loops); CN domains → local; A/AAAA → fakeip.
func (r *Renderer) buildDNS() map[string]any {
	servers := []map[string]any{
		{
			"tag":              "remote",
			"address":          r.cfg.DNS.ProxyDoH[0],
			"address_resolver": "local-bootstrap",
			"strategy":         "ipv4_only",
			"detour":           "out",
		},
		{
			"tag":              "local",
			"address":          r.cfg.DNS.CNDoH[0],
			"address_resolver": "local-bootstrap",
			"strategy":         "ipv4_only",
			"detour":           "direct",
		},
		{"tag": "fakeip", "address": "fakeip"},
		{
			"tag":     "local-bootstrap",
			"address": r.cfg.DNS.BootstrapResolver,
			"detour":  "direct",
		},
	}

	rules := []map[string]any{
		// Resolve sing-box's own outbound dialer queries (e.g. resolving the
		// airport node's own hostname) via local — avoids feeding our own
		// DoH dial back into fakeip and looping.
		{"outbound": "any", "server": "local"},
		// CN domains → local DoH (real IP, hits CN CDN edges).
		{"rule_set": []string{"geosite-cn"}, "server": "local"},
		// Everything else A/AAAA → fake-IP.
		{"query_type": []string{"A", "AAAA"}, "server": "fakeip"},
	}

	return map[string]any{
		"servers":           servers,
		"rules":             rules,
		"fakeip":            map[string]any{"enabled": true, "inet4_range": r.cfg.DNS.FakeIPRange},
		"strategy":          "ipv4_only",
		"independent_cache": true,
		"final":             "remote",
	}
}

// buildInbounds emits the TUN inbound (when node.tun0_gateway_ip is set) plus
// the optional ad-hoc socks/http inbounds. The TUN inbound listens on a private
// peer addr and is fed by external nft fwmark + ip rule (managed by leap-nft).
func (r *Renderer) buildInbounds() []map[string]any {
	var inbounds []map[string]any

	if r.cfg.TUN.Enabled && r.node.Tun0GatewayIP != "" {
		inbounds = append(inbounds, map[string]any{
			"type":           "tun",
			"tag":            "tun-in",
			"interface_name": r.cfg.TUN.InterfaceName,
			"address":        []string{r.cfg.TUN.Address},
			"auto_route":     false,
			"strict_route":   false,
			"stack":          "system",
			"sniff":          true,
		})
	}

	// DNS direct inbound — sing-box listens on tun0_gateway_ip:53 so FeiLian
	// clients (whose push-DNS points here) reach us directly. sniff=true
	// detects DNS protocol; route rule {protocol:dns, outbound:dns-out} then
	// hands off to sing-box's DNS subsystem.
	if r.node.Tun0GatewayIP != "" {
		dnsListen := r.cfg.DNS.Listen
		if dnsListen == "" {
			dnsListen = r.node.Tun0GatewayIP + ":53"
		}
		host, port := splitListen(dnsListen, 53)
		inbounds = append(inbounds, map[string]any{
			"type":        "direct",
			"tag":         "dns-in",
			"listen":      host,
			"listen_port": port,
			"sniff":       true,
		})
	}

	// Ad-hoc socks/http inbounds — kept for cmd/selftest and for poking the
	// gateway from the node itself when debugging.
	if r.cfg.SOCKSListen != "" {
		host, port := splitListen(r.cfg.SOCKSListen, 1080)
		inbounds = append(inbounds, map[string]any{
			"type":        "socks",
			"tag":         "socks-in",
			"listen":      host,
			"listen_port": port,
		})
	}
	if r.cfg.HTTPListen != "" {
		host, port := splitListen(r.cfg.HTTPListen, 1081)
		inbounds = append(inbounds, map[string]any{
			"type":        "http",
			"tag":         "http-in",
			"listen":      host,
			"listen_port": port,
		})
	}

	return inbounds
}

// buildOutbounds emits the outbound graph per design.md §3:
//
//	out (selector) → urltest → [airport nodes filtered by URLTest.NodePattern]
//	                          → direct
//	direct, dns-out, block, plus the airport node entries themselves.
//
// All parsed airport nodes are kept as defined outbounds (queryable via
// clash-api) regardless of the filter; only the urltest pool is restricted.
func (r *Renderer) buildOutbounds(outbounds []subscribe.Outbound) []map[string]any {
	tags := make([]string, 0, len(outbounds))
	for _, o := range outbounds {
		if t := o.Tag(); t != "" {
			tags = append(tags, t)
		}
	}

	urltestPool := tags
	if pattern := r.cfg.URLTest.NodePattern; pattern != "" {
		re, err := regexp.Compile(pattern)
		if err != nil {
			slog.Warn("urltest node_pattern is not a valid regexp; using all nodes",
				"pattern", pattern, "err", err)
		} else {
			filtered := make([]string, 0, len(tags))
			for _, t := range tags {
				if re.MatchString(t) {
					filtered = append(filtered, t)
				}
			}
			if len(filtered) == 0 && len(tags) > 0 {
				slog.Warn("urltest node_pattern matched no nodes; falling back to all",
					"pattern", pattern, "total_nodes", len(tags))
			} else {
				urltestPool = filtered
				slog.Info("urltest pool filtered",
					"pattern", pattern, "selected", len(filtered), "total", len(tags))
			}
		}
	}

	all := make([]map[string]any, 0, len(outbounds)+5)

	interval := r.cfg.URLTest.Interval.String()
	probeURL := r.cfg.URLTest.ProbeURL
	tolerance := r.cfg.URLTest.Tolerance

	if len(urltestPool) > 0 {
		all = append(all, map[string]any{
			"type":      "selector",
			"tag":       "out",
			"outbounds": append([]string{"urltest"}, "direct"),
			"default":   "urltest",
		})
		all = append(all, map[string]any{
			"type":      "urltest",
			"tag":       "urltest",
			"outbounds": urltestPool,
			"url":       probeURL,
			"interval":  interval,
			"tolerance": tolerance,
		})
	} else {
		// Bootstrap: no nodes parsed yet. Make "out" point to direct so the
		// rendered config is still loadable; sing-box will start, route.final
		// keeps working, just without proxy capability until a refresh
		// produces real outbounds.
		all = append(all, map[string]any{
			"type":      "selector",
			"tag":       "out",
			"outbounds": []string{"direct"},
			"default":   "direct",
		})
	}

	// Airport node outbounds — the parsed subscription entries verbatim.
	for _, o := range outbounds {
		all = append(all, map[string]any(o))
	}

	all = append(all,
		map[string]any{"type": "direct", "tag": "direct"},
		map[string]any{"type": "dns", "tag": "dns-out"},
		map[string]any{"type": "block", "tag": "block"},
	)

	return all
}

// buildRoute emits the route section per design.md §5: dns→dns-out, private/CN
// rule_set → direct, final → out (selector). Rule sets are remote, downloaded
// via the proxy detour (so the node itself doesn't need to reach
// raw.githubusercontent.com directly — that traverses GFW).
func (r *Renderer) buildRoute() map[string]any {
	ruleSet := []map[string]any{
		{
			"tag":              "geosite-cn",
			"type":             "remote",
			"format":           "binary",
			"url":              r.cfg.Route.GeositeURL,
			"download_detour":  "out",
			"update_interval":  "168h",
		},
		{
			"tag":              "geoip-cn",
			"type":             "remote",
			"format":           "binary",
			"url":              r.cfg.Route.GeoIPURL,
			"download_detour":  "out",
			"update_interval":  "168h",
		},
	}

	rules := []map[string]any{
		// DNS traffic landing on dns-in or sniffed as DNS → handled by sing-box's DNS subsystem.
		{"protocol": "dns", "outbound": "dns-out"},
		// Private / LAN ranges → never proxy.
		{"ip_is_private": true, "outbound": "direct"},
		// CN domains / IPs → direct.
		{"rule_set": []string{"geosite-cn", "geoip-cn"}, "outbound": "direct"},
	}

	return map[string]any{
		"rule_set":              ruleSet,
		"rules":                 rules,
		"final":                 "out",
		"auto_detect_interface": true,
	}
}

func (r *Renderer) buildExperimental() map[string]any {
	clashAPI := map[string]any{
		"external_controller": r.cfg.ClashAPI.ExternalController,
	}
	if r.cfg.ClashAPI.Secret != "" {
		clashAPI["secret"] = r.cfg.ClashAPI.Secret
	}
	return map[string]any{
		"clash_api":  clashAPI,
		"cache_file": map[string]any{"enabled": true, "store_fakeip": true},
	}
}

// splitListen accepts "host:port" or "host" and returns (host, port). On parse
// error it falls back to "0.0.0.0" and the provided default port.
func splitListen(addrPort string, defPort int) (string, int) {
	host, portStr, err := net.SplitHostPort(addrPort)
	if err != nil {
		// Treat the whole string as the host and use defPort.
		if addrPort == "" {
			return "0.0.0.0", defPort
		}
		return addrPort, defPort
	}
	if host == "" {
		host = "0.0.0.0"
	}
	var p int
	if _, err := fmt.Sscanf(portStr, "%d", &p); err != nil {
		return host, defPort
	}
	return host, p
}
