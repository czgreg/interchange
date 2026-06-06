// Package config defines the on-disk yaml schema (gateway.yaml) and applies
// defaults at load time. Schema lives in this single file — keep it that way
// so new fields are easy to find by reading top-to-bottom.
//
// History: leap-gateway started as a sing-box sidecar; the legacy schema
// nested all engine fields under `singbox:`. After the mihomo cutover and
// the sing-box renderer was removed, the top-level block was renamed to
// `data_plane:` and three new sibling blocks were added — `node_qualify`,
// `pools` — that used to be buried under `singbox.url_test.*`. The old
// `singbox:` and `urltest.watchdog:` blocks are no longer accepted.
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
	DataPlane     DataPlaneConfig     `yaml:"data_plane"`

	// NodeQualify drives the NodeScorer goroutine that dynamically manages
	// us-pool membership: each ScoringInterval the scorer probes every
	// candidate, applies the thresholds + probe checks, and hot-reloads
	// mihomo when the qualified set changes.
	NodeQualify NodeQualifyConfig `yaml:"node_qualify"`

	// Pools lists named select groups that get rendered alongside the
	// default us-pool. Each pool is gated by `requires_passing` (probe
	// names from NodeQualify.Probes that must all pass) and routes the
	// listed `rule_sets` via mihomo rules. Used to segregate sites with
	// special needs (e.g. openai-pool for ChatGPT/Claude — only nodes
	// passing chatgpt.com / claude.ai probes qualify).
	Pools []PoolConfig `yaml:"pools"`

	// Capacity holds the single-node user-capacity ceiling measured by an
	// offline stress test (scripts/stress.sh). These are STATIC operator-
	// supplied numbers — leap-gateway does not measure them at runtime
	// (you can't load-test live production). Surfaced in /api/status so
	// operators can compare current load against the known ceiling.
	Capacity CapacityConfig `yaml:"capacity"`

	// LoadBalance tunes how overseas (us-pool-bound) traffic is spread.
	// Default zero value = per-destination (consistent-hashing) as before.
	LoadBalance LoadBalanceConfig `yaml:"load_balance"`
}

// CapacityConfig records the stress-tested single-node ceiling. Update it
// after each re-run of scripts/stress.sh; bump measured_with when the
// binary or mihomo version changes (those invalidate the numbers).
type CapacityConfig struct {
	// SustainedMaxUsers is the concurrent-user count the node carries with
	// mihomo CPU < ~50% and no latency spike. Safe operating ceiling.
	SustainedMaxUsers int `yaml:"sustained_max_users"`
	// DegradedMaxUsers is where latency starts to spike but the node still
	// functions. Hard ceiling; do not exceed.
	DegradedMaxUsers int `yaml:"degraded_max_users"`
	// MeasuredAt is the date the stress test was run (free-form, e.g.
	// "2026-06-08"). Audit aid.
	MeasuredAt string `yaml:"measured_at"`
	// MeasuredWith is the gateway version / mihomo version the test ran
	// against (e.g. "bf6e610 / mihomo v1.19.26"). When the running version
	// differs, the numbers are stale — re-test.
	MeasuredWith string `yaml:"measured_with"`
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
	// Different airports gate by UA in incompatible ways — set this per
	// subscription to whatever UA the provider expects.
	UserAgent string `yaml:"user_agent,omitempty"`
}

// NodeConfig describes the FeiLian forwarding node that leap is sidecared on.
// All three fields are assigned by FeiLian SaaS when the node is added to the
// control plane. They cannot be guessed — the deploy operator fills them in.
type NodeConfig struct {
	// ClientSubnet is the CIDR of the client pool (e.g. "10.8.11.0/24").
	ClientSubnet string `yaml:"client_subnet"`
	// Tun0GatewayIP is the IP address mihomo's DNS server binds to
	// (e.g. "10.8.11.1"). It must be the gateway of tun0 — the address that
	// the FeiLian client sees as the gateway of its tunnel.
	Tun0GatewayIP string `yaml:"tun0_gateway_ip"`
	// EgressIface is the interface that the node uses to reach the public
	// internet (e.g. "ens18"). Used for diagnostic reporting.
	EgressIface string `yaml:"egress_iface"`
}

// DataPlaneConfig describes the proxy data plane (mihomo) the control plane
// renders to + reloads. mihomo is the only supported engine; the field name
// `data_plane` is engine-neutral by intent (so a future swap doesn't force
// another rename).
type DataPlaneConfig struct {
	// ConfigPath is where the renderer writes mihomo's config.yaml.
	// Defaults to /var/lib/leap/mihomo/config.yaml.
	ConfigPath string `yaml:"config_path"`

	LogLevel string `yaml:"log_level"`

	// RuleSetsDir holds .mrs files used by mihomo. Pre-fetched into the
	// install tarball by scripts/stage.sh; expanded at startup.
	// Defaults to /var/lib/leap/mihomo/rule-sets.
	RuleSetsDir string `yaml:"rule_sets_dir"`

	ClashAPI ClashAPIConfig `yaml:"clash_api"`

	// TUN drives mihomo's TUN inbound that catches forwarded traffic via
	// fwmark policy routing.
	TUN TUNConfig `yaml:"tun"`

	// DNS drives mihomo's DNS server upstreams.
	DNS DNSConfig `yaml:"dns"`

	// Route drives the rule-set URLs + whitelist mode.
	Route RouteConfig `yaml:"route"`

	// URLTest tunes mihomo's load-balance internal url-test. Note: this
	// is mihomo's own concept, not our NodeScorer — they're complementary.
	URLTest URLTestConfig `yaml:"url_test"`

	// TProxyPort is the port mihomo listens on for transparent-proxy
	// (TPROXY) inbound traffic. When non-zero, iproute.sh redirects
	// non-DNS TCP/UDP from the FeiLian client subnet to this port using
	// the kernel's TPROXY target, which preserves the original client
	// source IP (10.8.x.x) in mihomo's connection metadata.
	//
	// This is the ONLY mechanism that preserves real source IPs. TUN mode
	// (system OR gvisor) always reports 198.18.0.0 for all clients because
	// the network stack reconstructs TCP connections without propagating the
	// original IP header — confirmed on production node 89, 2026-06-06.
	//
	// per_terminal routing (load_balance.per_terminal) REQUIRES this field
	// to be set. Validation refuses tproxy_port=0 — TUN-only mode collapses
	// all clients to 198.18.0.0 and is no longer a supported deploy path.
	TProxyPort int `yaml:"tproxy_port"`

	// Legacy ad-hoc inbounds, retained for cmd/selftest. Empty = omitted.
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
	// Stack selects mihomo's TUN network stack:
	//   "system" (default) — kernel handles the TUN socket.
	//   "gvisor"           — userspace stack.
	//
	// IMPORTANT: neither stack preserves the real client sourceIP in mihomo's
	// connection metadata. Both collapse sourceIP to 198.18.0.0 (the TUN device
	// address) because the kernel/gvisor stack reconstructs the TCP connection
	// from raw TUN packets without propagating the original IP header.
	// Per-terminal routing requires TPROXY (data_plane.tproxy_port), not gvisor.
	Stack string `yaml:"stack"`
}

// LoadBalanceConfig tunes how overseas (us-pool-bound) traffic is spread.
type LoadBalanceConfig struct {
	// PerTerminal, when true, pins each client terminal's whitelisted
	// (domain-matched) traffic to a single egress node — "one terminal,
	// one egress". Eliminates the multi-node drift within one logical
	// session (chatgpt.com / chat.openai.com / ws.chatgpt.com all exit the
	// same IP) that trips OpenAI/Cloudflare anomaly detection.
	//
	// REQUIRES data_plane.tproxy_port to be set. TPROXY preserves the real
	// client sourceIP (10.8.x.x) so SRC-IP-CIDR slicing works. TUN mode
	// (tproxy_port=0) always collapses all clients to 198.18.0.0 regardless
	// of TUN stack (confirmed on production node 89, 2026-06-06).
	//
	// Mechanism: the renderer slices node.client_subnet into per-/32
	// SRC-IP-CIDR rules under a `perterm` sub-rule, assigning each terminal
	// to a qualified us-pool node via rendezvous (HRW) hashing. Whitelist
	// rules route to the perterm sub-rule instead of directly to `out`.
	PerTerminal bool `yaml:"per_terminal"`
}

type DNSConfig struct {
	// Listen overrides the default <Tun0GatewayIP>:53. Useful in lab.
	Listen string `yaml:"listen"`
	// FakeIPRange is the IPv4 fake-IP pool. Default 198.18.0.0/15.
	FakeIPRange string `yaml:"fakeip_range"`
	// CNDoH is the upstream pool for CN-side resolution.
	CNDoH []string `yaml:"cn_doh"`
	// ProxyDoH is the upstream pool for non-CN resolution. Routed through
	// mihomo's "out" selector → us-pool by the renderer.
	ProxyDoH []string `yaml:"proxy_doh"`
	// BootstrapResolver is an IP literal used to resolve any DoH/DoT host
	// names without chicken-and-egg (e.g. "udp://119.29.29.29").
	BootstrapResolver string `yaml:"bootstrap_resolver"`
	// PreloadDomains is the list of overseas domains leap-gateway will
	// keep warm in mihomo's DNS cache.
	PreloadDomains []string `yaml:"preload_domains,omitempty"`
	// PreloadInterval is how often each preloaded domain is re-queried.
	// Default 5m. 0 disables preload (regardless of PreloadDomains).
	PreloadInterval time.Duration `yaml:"preload_interval,omitempty"`
	// FakeIPSkipSuffixes lists domain suffixes that must NOT receive a
	// fake-IP. Required for internal/private services whose hostnames
	// are not on geosite-cn but resolve to CN/LAN IPs that should be
	// reached DIRECTly. Without this, mihomo's fake-ip mode hands out
	// 198.18.x.x and DIRECT outbound dials the unreachable fake IP.
	FakeIPSkipSuffixes []string `yaml:"fake_ip_skip_suffixes,omitempty"`
}

type RouteConfig struct {
	GeositeURL string `yaml:"geosite_url"`
	GeoIPURL   string `yaml:"geoip_url"`

	// Mode controls how non-CN traffic is split.
	//   "overseas" (default): every non-CN destination → us-pool. CN
	//                         domains/IPs still go DIRECT via geosite-cn /
	//                         geoip-cn. Most transparent to clients.
	//   "whitelist":          only domains hitting Whitelist.* go to
	//                         us-pool (or its named pool); everything else
	//                         goes DIRECT. Saves airport bandwidth at the
	//                         cost of curating the list.
	Mode string `yaml:"mode"`

	// Whitelist is consulted only when Mode == "whitelist".
	Whitelist WhitelistConfig `yaml:"whitelist"`
}

// WhitelistConfig enumerates what's allowed through the airport in
// whitelist mode. See docs/api.md for full semantics.
type WhitelistConfig struct {
	Geosites     StringList `yaml:"geosites"`
	Geoips       StringList `yaml:"geoips"`
	DomainSuffix []string   `yaml:"domain_suffix"`
	IPCIDR       []string   `yaml:"ip_cidr"`
	// FakeIPSkip is surfaced via PUT /api/whitelist as `fake_ip_skip`.
	// On mutation it is synced to cfg.DataPlane.DNS.FakeIPSkipSuffixes
	// so the renderer picks it up without a separate API call.
	FakeIPSkip []string `yaml:"fake_ip_skip"`
}

// StringList is yaml-decoded as []string but tolerates the legacy "list of
// {name, url}" mapping form so old gateway.yaml files keep loading.
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

// URLTestConfig drives mihomo's load-balance internal url-test. Distinct
// from NodeQualifyConfig (which is leap-gateway's own scorer); these
// thresholds shape mihomo's own per-flow health-check + url-test cycle.
type URLTestConfig struct {
	// Interval = mihomo url-test periodic full-pool re-test (default 3m).
	Interval time.Duration `yaml:"interval"`
	// Tolerance = ms hysteresis: only switch if a new candidate is faster
	// by this much (default 50). Mostly cosmetic under load-balance.
	Tolerance int `yaml:"tolerance"`
	// ProbeURL = URL probed for latency. Default gstatic.com/generate_204
	// because it must traverse GFW (which is what we want to verify).
	ProbeURL string `yaml:"probe_url"`
	// NodePattern is a Go-regexp; only outbounds whose tag matches are
	// added to us-pool. Empty = every airport node is a candidate.
	NodePattern string `yaml:"node_pattern"`
}

// NodeQualifyConfig is the threshold set for the NodeScorer. All thresholds
// must be satisfied simultaneously for a node to be considered "qualified"
// for the base us-pool. Probe results gate membership in named pools.
type NodeQualifyConfig struct {
	// Enabled turns the scorer on. Default true under mihomo.
	Enabled bool `yaml:"enabled"`
	// ScoringInterval is how often the scorer reads /proxies and
	// /connections and re-evaluates every node. Default 60s.
	ScoringInterval time.Duration `yaml:"scoring_interval"`
	// MaxRTTP50Ms: p50 RTT ceiling in ms. Default 500.
	MaxRTTP50Ms int `yaml:"max_rtt_p50_ms"`
	// MaxRTTP95Ms: p95 RTT ceiling in ms. Default 800.
	MaxRTTP95Ms int `yaml:"max_rtt_p95_ms"`
	// MaxJitterMs: p95-p50 spread in ms. Default 300.
	MaxJitterMs int `yaml:"max_jitter_ms"`
	// MaxFailRate: fraction of probes (delay=0 = failed) tolerated.
	// Default 0.25.
	MaxFailRate float64 `yaml:"max_fail_rate"`
	// MinProbes: minimum history entries before a node is eligible.
	// Default 2.
	MinProbes int `yaml:"min_probes"`
	// EvictStrikes: consecutive scoring rounds a node must fail before
	// being removed from the pool. Default 2.
	EvictStrikes int `yaml:"evict_strikes"`
	// ReadmitStrikes: consecutive passing rounds before an evicted node
	// is added back. Default 1.
	ReadmitStrikes int `yaml:"readmit_strikes"`

	// Probes defines per-target reachability checks (e.g. "can this node
	// load chatgpt.com without hitting Cloudflare's bot challenge?"). Each
	// probe runs from each candidate node periodically; results land in
	// NodeHealth.Probes and gate membership in named Pools.
	Probes []ProbeConfig `yaml:"probes"`
}

// ProbeConfig is one site-specific reachability check. Probes are NOT
// part of the basic RTT-qualification thresholds — a node failing probe
// X is removed only from pools that list X in `requires_passing`, not
// from the base us-pool.
type ProbeConfig struct {
	// Name is the probe identifier. Use the destination hostname so the
	// API surface is self-descriptive (e.g. "chatgpt.com", "claude.ai").
	// Pool config's `requires_passing` references probes by Name.
	Name string `yaml:"name"`
	// URL is the full HTTPS URL to GET. Caller's responsibility to use a
	// path that returns a stable response without auth.
	URL string `yaml:"url"`
	// Interval between probes per node. Default 5m. Lower values increase
	// detection speed at the cost of airport traffic + risk of being
	// flagged as bot probing.
	Interval time.Duration `yaml:"interval"`
	// Timeout caps each probe. Default 10s.
	Timeout time.Duration `yaml:"timeout"`
	// Check defines the pass criteria.
	Check ProbeCheckConfig `yaml:"check"`
}

// ProbeCheckConfig describes what counts as a probe pass. Initial rules
// are simple; if more become necessary they can be added as additional
// fields rather than replacing this with a generic rule list.
type ProbeCheckConfig struct {
	// MaxStatus is the inclusive upper bound for status_code = pass.
	// Default 399 (i.e. 1xx-3xx pass, 4xx/5xx fail).
	MaxStatus int `yaml:"max_status"`
	// RejectCFChallenge — if true, response carrying `cf-mitigated:
	// challenge` header is treated as failure even if status_code is in
	// range. Catches Cloudflare bot-challenge interstitials that return
	// 200 + HTML challenge body. Default false; set true on probes that
	// target CF-protected destinations (chatgpt.com, claude.ai).
	RejectCFChallenge bool `yaml:"reject_cf_challenge"`
}

// PoolConfig describes a named select group rendered alongside the default
// us-pool. Members are nodes that are both qualified for us-pool AND pass
// every probe listed in `requires_passing`. Routes the `rule_sets` to this
// pool instead of `out` (the default).
type PoolConfig struct {
	// Name is the pool tag emitted into mihomo's proxy-groups (e.g.
	// "openai-pool"). Must be unique across pools.
	Name string `yaml:"name"`
	// RequiresPassing names probes (from NodeQualify.Probes[].Name) that
	// every member must currently pass. Empty = no extra gating beyond
	// us-pool's basic RTT thresholds.
	RequiresPassing []string `yaml:"requires_passing"`
	// RuleSets names rule-providers (e.g. "geosite-openai", "geosite-
	// anthropic") that route through this pool instead of `out`. Each
	// must be present in cfg.DataPlane.Route.Whitelist.Geosites OR
	// auto-fetched.
	RuleSets []string `yaml:"rule_sets"`
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
		c.Subscribe.UserAgent = "ClashforWindows/0.20.39"
	}
	c.DataPlane.ApplyDefaults()
	c.NodeQualify.applyDefaults()
}

// ApplyDefaults fills in defaults for the DataPlane subtree. Public so
// callers that construct DataPlaneConfig directly (e.g. cmd/selftest) can
// invoke it.
func (c *DataPlaneConfig) ApplyDefaults() {
	if c.ConfigPath == "" {
		// mihomo workdir is /var/lib/leap/mihomo; mihomo reads config.yaml
		// from there automatically (no -f flag). Must match the path in
		// deploy/node/leap-mihomo.service (WorkingDirectory).
		c.ConfigPath = "/var/lib/leap/mihomo/config.yaml"
	}
	if c.RuleSetsDir == "" {
		c.RuleSetsDir = "/var/lib/leap/mihomo/rule-sets"
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
	if c.TUN.Stack == "" {
		c.TUN.Stack = "system"
	}

	// DNS defaults.
	if c.DNS.FakeIPRange == "" {
		c.DNS.FakeIPRange = "198.18.0.0/15"
	}
	if len(c.DNS.CNDoH) == 0 {
		c.DNS.CNDoH = []string{
			"https://doh.pub/dns-query",
			"https://dns.alidns.com/dns-query",
		}
	}
	if len(c.DNS.ProxyDoH) == 0 {
		c.DNS.ProxyDoH = []string{
			"tls://1.1.1.1:853",
			"https://doh.pub/dns-query",
		}
	}
	if c.DNS.BootstrapResolver == "" {
		c.DNS.BootstrapResolver = "udp://119.29.29.29"
	}
	if c.DNS.PreloadInterval == 0 && len(c.DNS.PreloadDomains) > 0 {
		c.DNS.PreloadInterval = 5 * time.Minute
	}

	// Route defaults — MetaCubeX/meta-rules-dat. mihomo loads .mrs from
	// the meta/ branch; the rulesets manager rewrites URLs at fetch time.
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
}

// applyDefaults fills NodeQualify defaults. NodeQualify is enabled by
// default — the scorer is core to mihomo's pool management.
func (c *NodeQualifyConfig) applyDefaults() {
	// Enabled defaults true. Bool defaults can't distinguish "user set
	// false" from "field missing", so explicit false is the way to
	// disable; absent field = enabled.
	if c.ScoringInterval == 0 {
		c.ScoringInterval = 60 * time.Second
		c.Enabled = true
	}
	if c.MaxRTTP50Ms == 0 {
		c.MaxRTTP50Ms = 500
	}
	if c.MaxRTTP95Ms == 0 {
		c.MaxRTTP95Ms = 800
	}
	if c.MaxJitterMs == 0 {
		c.MaxJitterMs = 300
	}
	if c.MaxFailRate == 0 {
		c.MaxFailRate = 0.25
	}
	if c.MinProbes == 0 {
		c.MinProbes = 2
	}
	if c.EvictStrikes == 0 {
		c.EvictStrikes = 2
	}
	if c.ReadmitStrikes == 0 {
		c.ReadmitStrikes = 1
	}
	for i := range c.Probes {
		if c.Probes[i].Interval == 0 {
			c.Probes[i].Interval = 5 * time.Minute
		}
		if c.Probes[i].Timeout == 0 {
			c.Probes[i].Timeout = 10 * time.Second
		}
		if c.Probes[i].Check.MaxStatus == 0 {
			c.Probes[i].Check.MaxStatus = 399
		}
	}
}
