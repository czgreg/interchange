package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	API           APIConfig           `yaml:"api"`
	Subscribe     SubscribeConfig     `yaml:"subscribe"`
	Subscriptions []SubscriptionEntry `yaml:"subscriptions"`
	Node          NodeConfig          `yaml:"node"`
	SingBox       SingBoxConfig       `yaml:"singbox"`
}

type APIConfig struct {
	Listen string `yaml:"listen"`
	Token  string `yaml:"token"`
}

type SubscribeConfig struct {
	RefreshInterval  time.Duration `yaml:"refresh_interval"`
	RefreshOnStartup bool          `yaml:"refresh_on_startup"`
	HTTPTimeout      time.Duration `yaml:"http_timeout"`
	UserAgent        string        `yaml:"user_agent"`
}

type SubscriptionEntry struct {
	Name    string `yaml:"name"`
	URL     string `yaml:"url"`
	Format  string `yaml:"format"` // auto | clash | singbox | uri | sip008
	Enabled bool   `yaml:"enabled"`
	// UserAgent overrides Subscribe.UserAgent for this subscription only.
	// Different airports gate by UA in incompatible ways:
	//   - yuyun (mhlnf.cn) returns full sing-box JSON for "sing-box/*" UA but
	//     a stripped Clash YAML with "proxies: []" for Clash UA.
	//   - ash (671234.xyz) returns HTTP 500 for "sing-box/*" UA but full
	//     Clash YAML for "ClashforWindows/*" UA.
	// Set this per subscription to whatever UA the provider expects;
	// leave empty to use the global default.
	UserAgent string `yaml:"user_agent,omitempty"`
}

// NodeConfig describes the FeiLian forwarding node that leap is sidecared on.
// All three fields are assigned by FeiLian SaaS when the node is added to the
// control plane. They cannot be guessed — the deploy operator fills them in.
type NodeConfig struct {
	// ClientSubnet is the CIDR of the client pool (e.g. "10.8.11.0/24").
	ClientSubnet string `yaml:"client_subnet"`
	// Tun0GatewayIP is the IP address sing-box DNS server binds to
	// (e.g. "10.8.11.1"). It must be the gateway of tun0 — the address that
	// the FeiLian client sees as the gateway of its tunnel.
	Tun0GatewayIP string `yaml:"tun0_gateway_ip"`
	// EgressIface is the interface that the node uses to reach the public
	// internet (e.g. "ens18"). Used for sing-box auto_detect_interface fallback.
	EgressIface string `yaml:"egress_iface"`
}

type SingBoxConfig struct {
	// Engine selects the proxy data plane:
	//   "sing-box" (default) — render config.json, restart leap-singbox.service
	//   "mihomo"             — render config.yaml, restart leap-mihomo.service
	// Both engines share the same internal Outbound representation; the
	// renderer + controller pick is driven by this field at startup.
	Engine string `yaml:"engine"`

	ConfigPath string `yaml:"config_path"`
	LogLevel   string `yaml:"log_level"`

	// RuleSetsDir is where the renderer expects .srs files (geosite-cn.srs,
	// geoip-cn.srs, plus any whitelist geosite). Files are baked into the
	// tarball by scripts/stage.sh and installed by deploy/node/install.sh —
	// they're loaded at startup with type=local so sing-box doesn't have to
	// race against urltest readiness during initial download.
	RuleSetsDir string `yaml:"rule_sets_dir"`

	// BinaryPath points at the sing-box executable. The control plane shells
	// out to it for `rule-set decompile` (used by whitelistexpand to turn
	// geoip-* .srs blobs into a flat IP/CIDR list for /api/whitelist/resolved).
	// The leap-singbox.service unit has its own ExecStart and isn't affected
	// by this — it's only used out-of-band by the leap-gateway process.
	BinaryPath string `yaml:"binary_path"`

	ClashAPI ClashAPIConfig `yaml:"clash_api"`

	// TUN drives the TUN inbound that catches forwarded traffic redirected to
	// us via fwmark policy routing.
	TUN TUNConfig `yaml:"tun"`

	// DNS drives the sing-box DNS server upstreams.
	DNS DNSConfig `yaml:"dns"`

	// Route drives the sing-box route.rule_set URLs.
	Route RouteConfig `yaml:"route"`

	// URLTest tunes the auto-selected outbound's behavior + the watchdog that
	// guards it.
	URLTest URLTestConfig `yaml:"urltest"`

	// Legacy ad-hoc inbounds, retained so cmd/selftest renders something
	// sing-box check can validate without a full deployment. If left empty,
	// these inbounds are simply omitted.
	SOCKSListen string `yaml:"socks_listen"`
	HTTPListen  string `yaml:"http_listen"`
}

type ClashAPIConfig struct {
	ExternalController string `yaml:"external_controller"`
	Secret             string `yaml:"secret"`
}

type TUNConfig struct {
	Enabled       bool   `yaml:"enabled"`
	InterfaceName string `yaml:"interface_name"`
	Address       string `yaml:"address"` // e.g. "172.19.0.1/30"
	FWMark        int    `yaml:"fwmark"`
	RoutingTable  int    `yaml:"routing_table"`
}

type DNSConfig struct {
	// Listen overrides the default <Tun0GatewayIP>:53. Useful in lab.
	Listen string `yaml:"listen"`
	// FakeIPRange is the IPv4 fake-IP pool. Default 198.18.0.0/15.
	FakeIPRange string `yaml:"fakeip_range"`
	// CNDoH is the upstream pool for CN-side resolution.
	CNDoH []string `yaml:"cn_doh"`
	// ProxyDoH is the upstream pool for non-CN resolution. The first entry
	// gets detour=out (goes through the airport proxy).
	ProxyDoH []string `yaml:"proxy_doh"`
	// BootstrapResolver is an IP literal used to resolve any DoH/DoT host
	// names without chicken-and-egg (e.g. "udp://119.29.29.29").
	BootstrapResolver string `yaml:"bootstrap_resolver"`
	// PreloadDomains is the list of overseas domains leap-gateway will
	// keep warm in sing-box's DNS cache. Set this to your team's frequent
	// destinations (claude.ai, anthropic.com, github.com, ...) so the
	// first FeiLian client to access each one doesn't pay the ~400ms
	// cross-border DoH latency. Empty disables preload.
	PreloadDomains []string `yaml:"preload_domains,omitempty"`
	// PreloadInterval is how often each preloaded domain is re-queried.
	// Should be smaller than the typical DNS TTL so the cache stays warm.
	// Default 5m. 0 disables preload (regardless of PreloadDomains).
	PreloadInterval time.Duration `yaml:"preload_interval,omitempty"`
	// FakeIPSkipSuffixes lists domain suffixes that must NOT receive a
	// fake-IP from the engine's DNS server. Required for internal/private
	// services whose hostnames are not on geosite-cn but resolve to CN /
	// LAN IPs that should be reached DIRECTly. Without this, mihomo's
	// fake-ip mode hands out 198.18.x.x for these hosts and the DIRECT
	// outbound dials the unreachable fake IP — destination times out.
	//
	// Use leading-dot or "+.<suffix>" form per mihomo convention; both
	// are accepted (renderer normalizes to "+." prefix).
	//
	// Example: ["+.paigod.work", "+.feilian.cn"]. The defaults +.lan,
	// +.local, +.cn are always emitted by the renderer regardless of
	// this list.
	FakeIPSkipSuffixes []string `yaml:"fake_ip_skip_suffixes,omitempty"`
}

type RouteConfig struct {
	GeositeURL string `yaml:"geosite_url"`
	GeoIPURL   string `yaml:"geoip_url"`

	// Mode controls how non-CN traffic is split.
	//
	//   "overseas" (default): every non-CN destination → urltest (airport).
	//                         CN domains/IPs still go direct via geosite-cn /
	//                         geoip-cn. This is the original behavior — most
	//                         transparent to clients.
	//   "whitelist":          only domains hitting Whitelist.* go to urltest;
	//                         everything else (including non-CN sites that
	//                         aren't whitelisted) goes direct. Saves airport
	//                         bandwidth at the cost of curating the list.
	//
	// Empty / unrecognized values fall back to "overseas".
	Mode string `yaml:"mode"`

	// Whitelist is consulted only when Mode == "whitelist".
	Whitelist WhitelistConfig `yaml:"whitelist"`
}

// WhitelistConfig enumerates what's allowed through the airport in whitelist
// mode. The four lists are unioned at render time:
//
//   - Domain-side: Geosites (rule_set refs) ∪ DomainSuffix (literal suffixes).
//     Hits via DNS / SNI sniff → outbound "out".
//   - IP-side: Geoips (rule_set refs) ∪ IPCIDR (literal CIDRs/IPs).
//     Hits via destination IP match → outbound "out". Critical for apps that
//     bypass DNS by hardcoding IPs (Telegram MTProto, Signal, ProtonVPN
//     bootstrap). Without these, the IP-direct connection falls through to
//     route.final=direct and gets GFW'd.
//
// Tag conventions:
//   - Geosites entries must start with "geosite-" (matches our internal tag
//     namespace; the upstream URL strips the prefix per {stem} convention).
//   - Geoips entries must start with "geoip-" (same convention).
//   - DomainSuffix: bare domains, no scheme/path (e.g. "claude.ai").
//   - IPCIDR: CIDR or bare IP. Bare IP normalized to /32 (v4) or /128 (v6).
type WhitelistConfig struct {
	Geosites     StringList `yaml:"geosites"`
	Geoips       StringList `yaml:"geoips"`
	DomainSuffix []string   `yaml:"domain_suffix"`
	IPCIDR       []string   `yaml:"ip_cidr"`
}

// StringList is yaml-decoded as []string but tolerates the legacy "list of
// {name, url}" mapping form so old gateway.yaml files keep loading. The URL
// field was never read at render time (rule-sets come off disk via stage.sh)
// and is dropped on first re-write by the configstore.
type StringList []string

func (s *StringList) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("expected list, got kind %d", node.Kind)
	}
	out := make([]string, 0, len(node.Content))
	for _, item := range node.Content {
		switch item.Kind {
		case yaml.ScalarNode:
			out = append(out, item.Value)
		case yaml.MappingNode:
			// Legacy {name: X, url: Y} — keep just the name.
			var m struct {
				Name string `yaml:"name"`
			}
			if err := item.Decode(&m); err != nil {
				return err
			}
			if m.Name != "" {
				out = append(out, m.Name)
			}
		default:
			return fmt.Errorf("unsupported yaml kind %d in StringList", item.Kind)
		}
	}
	*s = out
	return nil
}

// URLTestConfig drives both sing-box's native urltest outbound and the
// in-process watchdog that supplements urltest's coarse interval-based check
// with a fast-path failure detector.
type URLTestConfig struct {
	// Interval = sing-box urltest periodic full-pool re-test (default 3m).
	Interval time.Duration `yaml:"interval"`
	// Tolerance = ms hysteresis: only switch if a new candidate is faster by
	// this much (default 50).
	Tolerance int `yaml:"tolerance"`
	// ProbeURL = the URL probed for latency. Default gstatic.com/generate_204
	// because it must traverse GFW (which is what we want to verify).
	ProbeURL string `yaml:"probe_url"`
	// NodePattern is a Go-regexp; only outbounds whose tag matches are added
	// to the urltest pool. Empty = every airport node is a candidate.
	NodePattern string `yaml:"node_pattern"`

	Watchdog WatchdogConfig `yaml:"watchdog"`
}

// WatchdogConfig drives the in-process active health checker. Every Interval
// it probes the currently selected urltest member; FailThreshold consecutive
// failures triggers a forced full re-test (clash-api /proxies/urltest/delay)
// which makes sing-box re-pick the fastest reachable node.
type WatchdogConfig struct {
	Enabled       bool          `yaml:"enabled"`
	Interval      time.Duration `yaml:"interval"`       // default 5s
	Timeout       time.Duration `yaml:"timeout"`        // default 3s
	FailThreshold int           `yaml:"fail_threshold"` // default 3

	// JitterPercent randomizes Interval by ±N% per tick (default 20). Reduces
	// the "exactly every 5s" fingerprint visible from inside the airport.
	JitterPercent int `yaml:"jitter_percent"`

	// BackoffMax caps the inter-probe sleep when the connection has been
	// healthy for many consecutive ticks. Default 60s. Sequence with default
	// Interval=5s: 5 → 10 → 20 → 40 → 60. Any failure or selection change
	// resets to Interval. Set to 0 to disable backoff.
	BackoffMax time.Duration `yaml:"backoff_max"`

	// RealTrafficSkip: if true, skip a probe whenever clash-api /connections
	// reports any positive byte-delta on a connection routed through "out"
	// since the last poll. Real user traffic IS the health signal — no need
	// to spam synthetic probes. Default true; set explicit `false` to opt out.
	// Pointer so we can distinguish "unset" (→default true) from "set false".
	RealTrafficSkip *bool `yaml:"real_traffic_skip"`

	// PrimaryRecoveryInterval: when failed-over to backup, how often to
	// independently probe the primary's currently-selected node. Default 30m.
	PrimaryRecoveryInterval time.Duration `yaml:"primary_recovery_interval"`

	// PrimaryRecoveryThreshold: consecutive healthy probes on primary required
	// before switching back from backup. Default 3.
	PrimaryRecoveryThreshold int `yaml:"primary_recovery_threshold"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.API.Listen == "" {
		// 18080 (not 8080) — feilian-sentry on the forwarding node already
		// owns 127.0.0.1:8080.
		c.API.Listen = "127.0.0.1:18080"
	}
	if c.Subscribe.HTTPTimeout == 0 {
		c.Subscribe.HTTPTimeout = 30 * time.Second
	}
	if c.Subscribe.UserAgent == "" {
		// Default airport User-Agent. Most providers speak Clash; some (yuyun)
		// also speak native sing-box JSON and prefer "sing-box/*" UA — those
		// must override per-subscription via SubscriptionEntry.UserAgent.
		c.Subscribe.UserAgent = "ClashforWindows/0.20.39"
	}
	c.SingBox.ApplyDefaults()
}

// ApplyDefaults fills in defaults for the SingBox subtree. Public so callers
// that construct SingBoxConfig directly (e.g. cmd/selftest) can invoke it.
func (c *SingBoxConfig) ApplyDefaults() {
	if c.Engine == "" {
		c.Engine = "sing-box"
	}
	switch c.Engine {
	case "sing-box":
		if c.ConfigPath == "" {
			c.ConfigPath = "/etc/leap/singbox/config.json"
		}
		if c.RuleSetsDir == "" {
			c.RuleSetsDir = "/etc/leap/singbox/rule-sets"
		}
		if c.BinaryPath == "" {
			c.BinaryPath = "/usr/local/bin/sing-box"
		}
	case "mihomo":
		if c.ConfigPath == "" {
			c.ConfigPath = "/etc/leap/mihomo/config.yaml"
		}
		if c.RuleSetsDir == "" {
			c.RuleSetsDir = "/etc/leap/mihomo/rule-sets"
		}
		if c.BinaryPath == "" {
			c.BinaryPath = "/usr/local/bin/mihomo"
		}
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.ClashAPI.ExternalController == "" {
		c.ClashAPI.ExternalController = "127.0.0.1:9090"
	}

	// TUN defaults — only used when TUN.Enabled is true.
	if c.TUN.InterfaceName == "" {
		c.TUN.InterfaceName = "utun-leap"
	}
	if c.TUN.Address == "" {
		c.TUN.Address = "172.19.0.1/30"
	}
	if c.TUN.FWMark == 0 {
		c.TUN.FWMark = 0x42
	}
	if c.TUN.RoutingTable == 0 {
		c.TUN.RoutingTable = 100
	}

	// DNS defaults.
	if c.DNS.FakeIPRange == "" {
		c.DNS.FakeIPRange = "198.18.0.0/15"
	}
	if len(c.DNS.CNDoH) == 0 {
		// Tencent first (faster from FeiLian node measured 2026-05-27),
		// AliDNS as fallback.
		c.DNS.CNDoH = []string{
			"https://doh.pub/dns-query",
			"https://dns.alidns.com/dns-query",
		}
	}
	if len(c.DNS.ProxyDoH) == 0 {
		// Cloudflare DoT — node-direct reachable per measurement.
		// doh.pub fallback because it returns un-poisoned answers for
		// overseas domains (verified 2026-05-27).
		c.DNS.ProxyDoH = []string{
			"tls://1.1.1.1:853",
			"https://doh.pub/dns-query",
		}
	}
	if c.DNS.BootstrapResolver == "" {
		c.DNS.BootstrapResolver = "udp://119.29.29.29"
	}
	if c.DNS.PreloadInterval == 0 && len(c.DNS.PreloadDomains) > 0 {
		// Default refresh cadence: 5min. Most DoH responses come back with
		// TTL ≥ 60s; sing-box honors that. Re-querying every 5min keeps
		// every preloaded domain in cache without burning bandwidth.
		c.DNS.PreloadInterval = 5 * time.Minute
	}

	// Route defaults — MetaCubeX/meta-rules-dat (daily build, Loyalsoldier-
	// enhanced upstream). Single source for both geosite and geoip keeps the
	// fetch / cache / failure path uniform.
	if c.Route.GeositeURL == "" {
		c.Route.GeositeURL = "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/sing/geo/geosite/cn.srs"
	}
	if c.Route.GeoIPURL == "" {
		c.Route.GeoIPURL = "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/sing/geo/geoip/cn.srs"
	}
	if c.Route.Mode == "" {
		c.Route.Mode = "overseas"
	}

	// URLTest defaults.
	if c.URLTest.Interval == 0 {
		c.URLTest.Interval = 3 * time.Minute
	}
	if c.URLTest.Tolerance == 0 {
		c.URLTest.Tolerance = 50
	}
	if c.URLTest.ProbeURL == "" {
		c.URLTest.ProbeURL = "https://www.gstatic.com/generate_204"
	}
	// Watchdog defaults — enabled by default once urltest is in place.
	if c.URLTest.Watchdog.Interval == 0 {
		c.URLTest.Watchdog.Interval = 5 * time.Second
	}
	if c.URLTest.Watchdog.Timeout == 0 {
		c.URLTest.Watchdog.Timeout = 3 * time.Second
	}
	if c.URLTest.Watchdog.FailThreshold == 0 {
		c.URLTest.Watchdog.FailThreshold = 3
	}
	if c.URLTest.Watchdog.JitterPercent == 0 {
		c.URLTest.Watchdog.JitterPercent = 20
	}
	if c.URLTest.Watchdog.BackoffMax == 0 {
		c.URLTest.Watchdog.BackoffMax = 60 * time.Second
	}
	// RealTrafficSkip is bool* — nil means "user didn't set it, use default
	// true". Explicit false in yaml opts out.
	if c.URLTest.Watchdog.RealTrafficSkip == nil {
		t := true
		c.URLTest.Watchdog.RealTrafficSkip = &t
	}
	if c.URLTest.Watchdog.PrimaryRecoveryInterval == 0 {
		c.URLTest.Watchdog.PrimaryRecoveryInterval = 30 * time.Minute
	}
	if c.URLTest.Watchdog.PrimaryRecoveryThreshold == 0 {
		c.URLTest.Watchdog.PrimaryRecoveryThreshold = 3
	}
}
