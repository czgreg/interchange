package singbox

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/leap-gateway/leap-gateway/internal/config"
	"github.com/leap-gateway/leap-gateway/internal/subscribe"
)

// LeapInternalProxyPort is the port of the loopback-only HTTP inbound the
// renderer always emits for leap-gateway's own outbound use (rulesets
// fetcher transits this so .srs downloads go through the airport instead
// of hitting GFW-blocked raw.githubusercontent directly). This inbound
// follows the `out` selector, so .srs fetches use whichever pool the
// watchdog / operator has currently selected.
const LeapInternalProxyPort = 11080

// LeapInternalProxyURL is the full http:// URL form of LeapInternalProxyPort
// — passed to rulesets.New as the proxy URL.
const LeapInternalProxyURL = "http://127.0.0.1:11080"

// LeapInternalBackupProxyPort is the port of a second loopback-only HTTP
// inbound emitted only when the rendered config has both urltest-primary
// and urltest-backup pools (≥2 enabled subscriptions). A route rule pins
// traffic from this inbound directly to urltest-backup, bypassing the
// `out` selector — used by the egress probe in /api/proxies/active so the
// backup pool's egress IP can be sampled without flipping `out`.
const LeapInternalBackupProxyPort = 11081

// LeapInternalBackupProxyURL is the full http:// URL form for the backup-
// pinned probe inbound. Only valid when a backup pool is rendered.
const LeapInternalBackupProxyURL = "http://127.0.0.1:11081"

// LeapInternalBackupInboundTag is the route-table identifier for the backup-
// pinned proxy inbound. Used both by the renderer (inbound tag) and the
// route rule that pins it to urltest-backup.
const LeapInternalBackupInboundTag = "leap-internal-http-backup-in"

// LeapInternalPrimaryProxyPort is a third loopback-only HTTP inbound, always
// emitted, pinned via route rule to urltest-primary. Egress-probe-only —
// distinct from LeapInternalProxyPort which follows `out` and would
// transit whichever pool watchdog/operator has currently selected
// (defeating the purpose of "what's primary's egress IP" if `out` happens
// to be on backup).
const LeapInternalPrimaryProxyPort = 11082

// LeapInternalPrimaryProxyURL is the full http:// URL form for the primary-
// pinned probe inbound.
const LeapInternalPrimaryProxyURL = "http://127.0.0.1:11082"

// LeapInternalPrimaryInboundTag is the route-table identifier for the
// primary-pinned proxy inbound.
const LeapInternalPrimaryInboundTag = "leap-internal-http-primary-in"

// Renderer turns a list of subscription outbounds + node config into a
// complete sing-box config.json. The output is suitable for `sing-box check`
// and for direct `sing-box run` after the node-side ip rule + nft injection.
type Renderer struct {
	cfg  config.SingBoxConfig
	node config.NodeConfig
	subs []config.SubscriptionEntry
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

// WithSubscriptions captures the configured subscription order so the renderer
// can split the urltest pool into urltest-primary (first enabled subscription's
// nodes) and urltest-backup (remaining enabled subs). Only takes effect when
// ≥2 subscriptions are enabled — single-sub deployments still emit a single
// urltest with tag "urltest-primary".
func (r *Renderer) WithSubscriptions(subs []config.SubscriptionEntry) *Renderer {
	r.SetSubscriptions(subs)
	return r
}

// SetSubscriptions replaces the subscription order in place — used by the
// management API after a CRUD edit, before re-rendering. Defensive copy so
// later mutations to the caller's slice don't leak into render-time logic.
func (r *Renderer) SetSubscriptions(subs []config.SubscriptionEntry) {
	cp := make([]config.SubscriptionEntry, len(subs))
	copy(cp, subs)
	r.subs = cp
}

// SetWhitelist re-seats the renderer's route mode + whitelist snapshot.
// Required because Renderer holds cfg by value: mutations to the live
// *config.Config don't propagate. Call from API handlers right before
// re-rendering after a whitelist edit, otherwise rule-set / route entries
// will drop newly added tags.
func (r *Renderer) SetWhitelist(mode string, wl config.WhitelistConfig) {
	r.cfg.Route.Mode = mode
	r.cfg.Route.Whitelist = wl
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
// rules and final depend on Route.Mode:
//
//	overseas (default): A/AAAA → fakeip; CN domains → local; final → remote.
//	                    Mirror of route: every non-CN destination goes proxy.
//	whitelist:          fakeip only for whitelisted geosites + domain_suffix;
//	                    final → local. Mirror of route: non-WL → direct, so
//	                    they need real-IP resolution (a fakeip handed to a
//	                    direct outbound is unreachable on the public internet).
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
	}

	final := "remote"
	if r.cfg.Route.Mode == "whitelist" {
		// Only WL hits get fakeip; everything else falls through to local.
		// Note: geoip / ip_cidr are NOT used here — DNS rules only operate on
		// domains. IP-side whitelist entries take effect at the route layer
		// when an app skips DNS entirely (e.g. Telegram MTProto).
		if len(r.cfg.Route.Whitelist.Geosites) > 0 {
			rules = append(rules, map[string]any{"rule_set": []string(r.cfg.Route.Whitelist.Geosites), "server": "fakeip"})
		}
		if sx := r.cfg.Route.Whitelist.DomainSuffix; len(sx) > 0 {
			rules = append(rules, map[string]any{"domain_suffix": sx, "server": "fakeip"})
		}
		final = "local"
	} else {
		// overseas mode: every A/AAAA that isn't CN → fakeip; everything else → remote.
		rules = append(rules, map[string]any{"query_type": []string{"A", "AAAA"}, "server": "fakeip"})
	}

	return map[string]any{
		"servers":           servers,
		"rules":             rules,
		"fakeip":            map[string]any{"enabled": true, "inet4_range": r.cfg.DNS.FakeIPRange},
		"strategy":          "ipv4_only",
		"independent_cache": true,
		"final":             final,
	}
}

// buildInbounds emits the TUN inbound (when node.tun0_gateway_ip is set) plus
// the optional ad-hoc socks/http inbounds. The TUN inbound listens on a private
// peer addr and is fed by external nft fwmark + ip rule (managed by leap-nft).
func (r *Renderer) buildInbounds() []map[string]any {
	var inbounds []map[string]any

	if r.cfg.TUN.Enabled && r.node.Tun0GatewayIP != "" {
		inbounds = append(inbounds, map[string]any{
			"type":                       "tun",
			"tag":                        "tun-in",
			"interface_name":             r.cfg.TUN.InterfaceName,
			"address":                    []string{r.cfg.TUN.Address},
			"auto_route":                 false,
			"strict_route":               false,
			"stack":                      "system",
			"sniff":                      true,
			"sniff_override_destination": true,
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

	// Internal HTTP proxy used by leap-gateway itself for outbound HTTP
	// fetches (rulesets.Manager downloads .srs blobs through this so they
	// transit the airport, since raw.githubusercontent is GFW-blocked from
	// inside CN). Loopback-only. Always present so operators don't have to
	// configure anything for on-demand rule-set fetching to work.
	inbounds = append(inbounds, map[string]any{
		"type":        "http",
		"tag":         "leap-internal-http-in",
		"listen":      "127.0.0.1",
		"listen_port": LeapInternalProxyPort,
	})

	// Primary-pinned probe inbound — always emitted (urltest-primary always
	// exists when there's at least one enabled subscription). Egress-probe-
	// only: pinned to urltest-primary by a route rule so the probe samples
	// primary's exit IP regardless of which pool watchdog has currently
	// selected.
	inbounds = append(inbounds, map[string]any{
		"type":        "http",
		"tag":         LeapInternalPrimaryInboundTag,
		"listen":      "127.0.0.1",
		"listen_port": LeapInternalPrimaryProxyPort,
	})

	// Backup-pinned probe inbound — only when a backup pool is rendered.
	// The egress probe in /api/proxies/active dials through this to sample
	// urltest-backup's exit IP without flipping the `out` selector. A
	// matching route rule (see buildRoute) pins it to urltest-backup.
	if _, hasBackup := r.primarySubscription(); hasBackup {
		inbounds = append(inbounds, map[string]any{
			"type":        "http",
			"tag":         LeapInternalBackupInboundTag,
			"listen":      "127.0.0.1",
			"listen_port": LeapInternalBackupProxyPort,
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

// buildOutbounds emits the outbound graph per design.md §3.
//
// One enabled subscription:
//
//	out (selector) → urltest-primary → [airport nodes filtered by NodePattern]
//	                                  → direct
//
// Two or more enabled subscriptions:
//
//	out (selector) → urltest-primary → [first sub's nodes, NodePattern-filtered]
//	              → urltest-backup  → [other subs' nodes, NodePattern-filtered]
//	              → direct
//
// All parsed airport nodes are kept as defined outbounds (queryable via
// clash-api) regardless of which urltest they land in. The watchdog flips the
// "out" selector between urltest-primary and urltest-backup when the primary's
// active node fails enough times in a row.
func (r *Renderer) buildOutbounds(outbounds []subscribe.Outbound) []map[string]any {
	primaryName, hasBackup := r.primarySubscription()
	primaryTags, backupTags := r.splitTagsBySub(outbounds, primaryName, hasBackup)

	primaryPool := r.applyNodePattern(primaryTags, "urltest-primary")
	var backupPool []string
	if hasBackup {
		backupPool = r.applyNodePattern(backupTags, "urltest-backup")
	}

	all := make([]map[string]any, 0, len(outbounds)+6)

	interval := r.cfg.URLTest.Interval.String()
	probeURL := r.cfg.URLTest.ProbeURL
	tolerance := r.cfg.URLTest.Tolerance

	switch {
	case len(primaryPool) > 0 && len(backupPool) > 0:
		all = append(all, map[string]any{
			"type":      "selector",
			"tag":       "out",
			"outbounds": []string{"urltest-primary", "urltest-backup", "direct"},
			"default":   "urltest-primary",
		})
		all = append(all, urltestEntry("urltest-primary", primaryPool, probeURL, interval, tolerance))
		all = append(all, urltestEntry("urltest-backup", backupPool, probeURL, interval, tolerance))
	case len(primaryPool) > 0:
		all = append(all, map[string]any{
			"type":      "selector",
			"tag":       "out",
			"outbounds": []string{"urltest-primary", "direct"},
			"default":   "urltest-primary",
		})
		all = append(all, urltestEntry("urltest-primary", primaryPool, probeURL, interval, tolerance))
	default:
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

// primarySubscription returns the name of the first enabled subscription and
// whether at least one other enabled subscription exists (i.e. backup pool is
// possible). Returns ("", false) when no subscription order info is available
// — falls back to "all nodes go to primary" semantics.
func (r *Renderer) primarySubscription() (name string, hasBackup bool) {
	enabled := 0
	for _, s := range r.subs {
		if !s.Enabled {
			continue
		}
		enabled++
		if name == "" {
			name = s.Name
		}
	}
	return name, enabled >= 2
}

// splitTagsBySub partitions outbound tags by the subscription-name prefix
// (`<sub_name>/...` per parser.go ParseBytes). When primaryName is empty —
// i.e. WithSubscriptions wasn't called — every tag goes to the primary pool.
func (r *Renderer) splitTagsBySub(outbounds []subscribe.Outbound, primaryName string, hasBackup bool) (primary, backup []string) {
	for _, o := range outbounds {
		t := o.Tag()
		if t == "" {
			continue
		}
		if primaryName == "" || !hasBackup {
			primary = append(primary, t)
			continue
		}
		prefix := primaryName + "/"
		if strings.HasPrefix(t, prefix) {
			primary = append(primary, t)
		} else {
			backup = append(backup, t)
		}
	}
	return primary, backup
}

// applyNodePattern compiles URLTest.NodePattern and returns the subset of tags
// that match. label is for log clarity ("urltest-primary" / "urltest-backup").
// On no-match-but-non-empty-input, returns the original (so the pool is never
// silently emptied by a too-strict pattern).
func (r *Renderer) applyNodePattern(tags []string, label string) []string {
	pattern := r.cfg.URLTest.NodePattern
	if pattern == "" || len(tags) == 0 {
		return tags
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		slog.Warn("urltest node_pattern is not a valid regexp; using all nodes",
			"pool", label, "pattern", pattern, "err", err)
		return tags
	}
	filtered := make([]string, 0, len(tags))
	for _, t := range tags {
		if re.MatchString(t) {
			filtered = append(filtered, t)
		}
	}
	if len(filtered) == 0 {
		slog.Warn("urltest node_pattern matched no nodes; falling back to all",
			"pool", label, "pattern", pattern, "total_nodes", len(tags))
		return tags
	}
	slog.Info("urltest pool filtered",
		"pool", label, "pattern", pattern, "selected", len(filtered), "total", len(tags))
	return filtered
}

func urltestEntry(tag string, pool []string, probeURL, interval string, tolerance int) map[string]any {
	return map[string]any{
		"type":      "urltest",
		"tag":       tag,
		"outbounds": pool,
		"url":       probeURL,
		"interval":  interval,
		"tolerance": tolerance,
	}
}

// buildRoute emits the route section. Two modes:
//
//	overseas (default): dns→dns-out, private/CN → direct, final → out (selector).
//	                    Every non-CN destination goes through urltest.
//	whitelist:          dns→dns-out, private/CN → direct, WL hits → out,
//	                    final → direct. Only whitelisted destinations egress
//	                    via airport; everything else goes direct.
//
// Rule sets are loaded as type=local from RuleSetsDir. .srs files are baked
// into the deploy tarball by scripts/stage.sh, sidestepping the GFW-blocked
// raw.githubusercontent.com path AND the startup race where 14 parallel
// remote downloads compete with urltest's first measurement window.
func (r *Renderer) buildRoute() map[string]any {
	dir := r.cfg.RuleSetsDir
	ruleSet := []map[string]any{
		ruleSetLocal("geosite-cn", dir),
		ruleSetLocal("geoip-cn", dir),
	}

	rules := []map[string]any{
		{"protocol": "dns", "outbound": "dns-out"},
	}

	// Egress-probe pinned inbounds bypass CN/private/WL classification
	// entirely — they only carry traffic to ipinfo.io / 1.1.1.1 to sample
	// the pool's exit IP. Both rules go BEFORE the private/CN block so the
	// probe transits the airport even if the destination matched something
	// like "private" by some quirk.
	rules = append(rules, map[string]any{
		"inbound":  []string{LeapInternalPrimaryInboundTag},
		"outbound": "urltest-primary",
	})
	if _, hasBackup := r.primarySubscription(); hasBackup {
		rules = append(rules, map[string]any{
			"inbound":  []string{LeapInternalBackupInboundTag},
			"outbound": "urltest-backup",
		})
	}

	rules = append(rules,
		map[string]any{"ip_is_private": true, "outbound": "direct"},
		map[string]any{"rule_set": []string{"geosite-cn", "geoip-cn"}, "outbound": "direct"},
	)

	final := "out"
	if r.cfg.Route.Mode == "whitelist" {
		wl := r.cfg.Route.Whitelist
		// Domain rule-sets first (geosite-*) — sing-box loads each as a separate
		// .srs file via type=local, no upstream fetch.
		for _, name := range wl.Geosites {
			ruleSet = append(ruleSet, ruleSetLocal(name, dir))
		}
		// IP rule-sets (geoip-*) — same shape on disk, different upstream repo.
		// Critical for apps that hardcode DC IPs and skip DNS entirely
		// (Telegram MTProto, Signal, ProtonVPN bootstrap).
		for _, name := range wl.Geoips {
			ruleSet = append(ruleSet, ruleSetLocal(name, dir))
		}
		if len(wl.Geosites) > 0 {
			rules = append(rules, map[string]any{"rule_set": []string(wl.Geosites), "outbound": "out"})
		}
		if len(wl.DomainSuffix) > 0 {
			rules = append(rules, map[string]any{"domain_suffix": wl.DomainSuffix, "outbound": "out"})
		}
		if len(wl.Geoips) > 0 {
			rules = append(rules, map[string]any{"rule_set": []string(wl.Geoips), "outbound": "out"})
		}
		if len(wl.IPCIDR) > 0 {
			rules = append(rules, map[string]any{"ip_cidr": wl.IPCIDR, "outbound": "out"})
		}
		// Footgun guard: a whitelist with nothing in it would route everything
		// to direct, defeating the point of running sing-box. Warn but proceed
		// — operator may be in transition.
		if len(wl.Geosites) == 0 && len(wl.DomainSuffix) == 0 && len(wl.Geoips) == 0 && len(wl.IPCIDR) == 0 {
			slog.Warn("route.mode=whitelist but whitelist is empty; all overseas traffic will go direct")
		}
		final = "direct"
	}

	return map[string]any{
		"rule_set":              ruleSet,
		"rules":                 rules,
		"final":                 final,
		"auto_detect_interface": true,
	}
}

func ruleSetLocal(tag, dir string) map[string]any {
	return map[string]any{
		"tag":    tag,
		"type":   "local",
		"format": "binary",
		"path":   dir + "/" + tag + ".srs",
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
