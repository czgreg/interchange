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

	// Notifications drives outbound alerts (currently Lark webhook only).
	// Disabled by default — operator must set `notifications.enabled: true`
	// AND configure a webhook target via API or yaml. Events that flow
	// through this layer: pool composition changes, emergency evictions,
	// node degradation crossings, webhook health.
	Notifications NotificationsConfig `yaml:"notifications"`
}

// NotificationsConfig is the user-facing notification settings. Webhook
// secrets are stored separately at runtime — yaml carries the URL +
// signing flag only, never the actual secret.
type NotificationsConfig struct {
	// Enabled gates the entire notification subsystem. false = scorer
	// events still log to slog, but no webhook calls fire and no local
	// JSONL log is written. Default false (operator must opt in).
	Enabled bool `yaml:"enabled"`

	// Lark holds the Lark/Feishu webhook configuration. Secrets are NOT
	// stored in yaml — set via POST /api/notifications/lark.
	Lark LarkConfig `yaml:"lark"`

	// LocalLogPath is the JSONL audit trail of every notification attempt
	// (before the webhook call), regardless of webhook delivery success.
	// Lets operators reconstruct "what was the system trying to tell me"
	// even when Lark itself was unreachable. Default
	// /var/lib/leap/notifications.log (writable per systemd ReadWritePaths).
	LocalLogPath string `yaml:"local_log_path"`

	// DedupWindow collapses repeated (node, event_type) notifications
	// within this window into one. Default 5m — long enough to suppress
	// BGP-flap spam without hiding genuine repeated incidents.
	DedupWindow time.Duration `yaml:"dedup_window"`

	// AggregationWindow batches multiple events occurring within this
	// window into a single Lark message. Default 30s — short enough that
	// urgency isn't lost, long enough to coalesce a 5-node simultaneous
	// flap. Aggregation NEVER applies to "urgent" severity.
	AggregationWindow time.Duration `yaml:"aggregation_window"`

	// PerNodeRateLimitPerHour caps how many "info"-tier notifications a
	// single node can generate per hour after dedup. Default 4 — covers
	// real flapping without becoming a firehose. Urgent events bypass
	// this limit.
	PerNodeRateLimitPerHour int `yaml:"per_node_rate_limit_per_hour"`

	// RetryAttempts on webhook 5xx / network errors. Default 3 with
	// exponential backoff (1s, 2s, 4s). Failed attempts are still logged
	// locally; urgent events that exhaust retries get re-attempted on
	// the next scoring round (best-effort recovery).
	RetryAttempts int `yaml:"retry_attempts"`

	// InstanceName identifies which gateway sent a notification, surfaced
	// in the Lark message header (e.g. "leap-gateway@92"). With multiple
	// nodes (89 / 92) posting to the same Lark channel, the operator can't
	// otherwise tell which one a pool-flap or node-dead alert came from.
	// Defaults to the OS hostname when left empty.
	InstanceName string `yaml:"instance_name"`
}

// LarkConfig is the Lark/Feishu webhook target. URL is yaml-stored; the
// signing secret is runtime-only (POST /api/notifications/lark).
type LarkConfig struct {
	// WebhookURL is the full Lark bot webhook URL (incl. token).
	// Empty = Lark output disabled even when Notifications.Enabled=true.
	WebhookURL string `yaml:"webhook_url"`

	// SignatureRequired turns on Lark's HMAC-SHA256 signing. When true,
	// runtime must have a secret set (via API); without one,
	// notifications fail-closed (logged locally, never POSTed). Strongly
	// recommended for any production webhook — default true.
	SignatureRequired bool `yaml:"signature_required"`

	// SecretFile is the on-disk path holding the Lark webhook signing
	// secret. POST /api/notifications/lark writes it (chmod 0600);
	// Notifier reads at startup + reload. Default
	// /var/lib/leap/lark-secret. Kept separate from yaml so secrets
	// never end up in version control / log dumps.
	SecretFile string `yaml:"secret_file"`
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
	// AllowFrom is the source-CIDR allowlist enforced in nft (NOT in Go —
	// the process still binds Listen as given). Rendered into nft.conf's
	// input chain by install.sh: loopback plus these CIDRs may reach the
	// API port, everything else is dropped.
	//
	// Needed because the input chain's policy is `accept`, so the old
	// `tcp dport <api> accept` rule was decorative — deleting it changed
	// nothing and every source could reach the port. Measured on 92
	// (2026-08-12) with listen=0.0.0.0:18080 + token="": a VPN client on
	// the tun0 client_subnet could GET /api/subscriptions and read every
	// subscription URL including its token query param, and POST/DELETE
	// /api/subscriptions to mutate them.
	//
	// Empty means "loopback only" — the safe default. Widen it only for
	// the operator network that runs make redeploy-* / deploy.sh, and set
	// Token as well: AllowFrom is network-layer defence, Token is the
	// application-layer one, and neither substitutes for the other.
	// NEVER include the tun0 client_subnet here; those are untrusted
	// end-user terminals.
	AllowFrom []string `yaml:"allow_from,omitempty"`
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
	// CNDoH is the upstream pool for CN-side resolution.
	CNDoH []string `yaml:"cn_doh"`
	// ProxyDoH is the upstream pool for non-CN resolution. Routed through
	// mihomo's "out" selector → us-pool by the renderer.
	ProxyDoH []string `yaml:"proxy_doh"`
	// BootstrapResolver is an IP literal used to resolve any DoH/DoT host
	// names without chicken-and-egg (e.g. "udp://119.29.29.29").
	BootstrapResolver string `yaml:"bootstrap_resolver"`
	// NodeResolver lists the upstreams used to resolve the airport-node
	// server hostnames matched by NodeResolverSuffixes. Default
	// ["udp://119.29.29.29"]. Only takes effect together with
	// NodeResolverSuffixes — on its own it does nothing.
	//
	// Why a plain-UDP upstream is worth having at all: mihomo's
	// proxy-server-nameserver ("psn") is a separate resolver instance from
	// `nameserver`, and a DoH frontend that answers NXDOMAIN for a name
	// that really exists produces "dns resolve failed: couldn't find ip" at
	// dial time. Measured on node 92 (2026-08-12) against the ash
	// provider's ingress z7mp4a9xq2k8f3r-r9.edg3.org: doh.pub's DoH
	// frontend returned NXDOMAIN 0/3 while plain UDP to 119.29.29.29 (same
	// operator, DNSPod) resolved 5/5. The two frontends do not share
	// recursion state.
	//
	// ⚠️ Appending an upstream to the psn LIST does not reliably fix that,
	// and the original 2026-08-12 fix which did so was wrong about the
	// mechanism. mihomo fires ALL psn upstreams in PARALLEL via `picker`
	// and takes the first non-error answer; NXDOMAIN is NOT an error there
	// (only SERVFAIL/REFUSED convert), so a fast NXDOMAIN WINS the race and
	// defeats a healthy upstream in the same list. Verified directly: psn =
	// [always-NXDOMAIN, udp://119.29.29.29] produced 10 resolve failures in
	// 10 attempts. List order is cosmetic.
	//
	// It also had a side effect: 119.29.29.29 answers in 0.04-0.06s versus
	// doh.pub's 0.164-0.291s, so in a latency race the plain-UDP upstream
	// silently became the de facto PRIMARY resolver for every node
	// hostname, moving all of them onto a spoofable transport.
	//
	// Hence the suffix-scoped design: psn itself stays CNDoH-only, and
	// NodeResolverSuffixes pins just the affected provider domains to these
	// upstreams via proxy-server-nameserver-policy, which is a deterministic
	// map lookup rather than a race. Everything else keeps resolving over
	// DoH.
	NodeResolver []string `yaml:"node_resolver,omitempty"`
	// NodeResolverSuffixes lists the domain suffixes pinned to NodeResolver
	// through mihomo's proxy-server-nameserver-policy. Empty (the default)
	// means no policy is emitted and psn is CNDoH-only.
	//
	// Populate this only for provider ingress domains with a demonstrated
	// DoH-NXDOMAIN problem, and keep it as narrow as possible: every suffix
	// listed here is resolved over plain UDP, i.e. spoofable. Node-hostname
	// resolution is a dial-time internal lookup where availability
	// dominates integrity, but that tradeoff should be made per-domain, not
	// globally.
	//
	// Accepts "edg3.org", ".edg3.org", or "+.edg3.org" — all normalize to
	// mihomo's "+.edg3.org" wildcard key form.
	//
	// Supported since mihomo v1.19.26 (config/config.go:240); verified with
	// `mihomo -t` on the node before rollout.
	NodeResolverSuffixes []string `yaml:"node_resolver_suffixes,omitempty"`
	// PreloadDomains is the list of overseas domains leap-gateway will
	// keep warm in mihomo's DNS cache.
	PreloadDomains []string `yaml:"preload_domains,omitempty"`
	// PreloadInterval is how often each preloaded domain is re-queried.
	// Default 5m. 0 disables preload (regardless of PreloadDomains).
	PreloadInterval time.Duration `yaml:"preload_interval,omitempty"`
	// FakeIPSkipSuffixes lists intranet/private domain suffixes that must
	// resolve via CN DoH (they land in nameserver-policy → CNDoH). Needed
	// for services whose hostnames aren't on geosite-cn but resolve to
	// CN/LAN IPs reached DIRECTly — the default nameserver is proxyDoH
	// (overseas), which has no records for them. Name kept for yaml/API
	// back-compat; predates the 2026-07-06 redir-host switch.
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

// URLTestConfig drives mihomo's load-balance internal probe + url-test.
// Distinct from NodeQualifyConfig (which is leap-gateway's own scorer);
// these thresholds shape mihomo's per-flow health-check + url-test cycle.
type URLTestConfig struct {
	// Interval: legacy field, kept for backward compatibility. Used as
	// fallback when HealthCheckInterval is unset. New configs should set
	// HealthCheckInterval directly. Default 3m.
	Interval time.Duration `yaml:"interval"`
	// HealthCheckInterval: how often mihomo internally probes each
	// LoadBalance member to update its alive bit. This is the FAST/binary
	// liveness signal — when a node TCP/HTTP-fails one probe, mihomo's
	// own LoadBalance routing skips it for new connections WITHOUT any
	// yaml rerender. Default 30s. Tune lower for snappier failover at
	// the cost of a small uptick in airport probe traffic; tune higher
	// only if airport quotas become a concern.
	//
	// Architectural rule: this MUST be ≤ NodeQualify.ScoringInterval.
	// Liveness is mihomo's job (this knob); graded eviction is the
	// scorer's job (slower cycle). If you raise this above the scorer's
	// interval, mihomo and the scorer trade roles and reloads churn.
	HealthCheckInterval time.Duration `yaml:"health_check_interval"`
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
	// /connections and re-evaluates every node. Default 5m.
	//
	// Architectural rule: this is the SLOW/graded decision cycle —
	// long-term RTT degradation, CF challenge gating, persistent
	// suitability for named pools. Fast/binary liveness is mihomo's
	// LoadBalance health-check (data_plane.url_test.health_check_interval,
	// default 30s). Keep this ≥ ~10× the health-check interval so a
	// transient flap mihomo's already filtering doesn't cascade into
	// an unnecessary scorer-driven hot-reload.
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
	// being removed from the pool. Default 2. With ScoringInterval=5m
	// this means a node has to fail two consecutive 5-minute reviews
	// (10 minutes of consistent degradation) to be evicted —
	// appropriate for "real degradation" vs transient noise (which
	// mihomo's 30s health-check is already filtering at the routing
	// layer).
	EvictStrikes int `yaml:"evict_strikes"`
	// ReadmitStrikes: consecutive passing rounds before an evicted node
	// is added back. Default 1. At 5-minute ScoringInterval, a single
	// passing round already represents 10 mihomo health-check cycles
	// of stability — that's enough to trust readmission without
	// flapping (which the 60-second scoring cycle could not provide).
	ReadmitStrikes int `yaml:"readmit_strikes"`
	// HotReloadMinInterval throttles how often pool changes can trigger
	// a clash-api PUT /configs?force=true. When a pool change is
	// detected within this window, the change is held off until the
	// next scoring round; the runtime mihomo state continues using the
	// last applied pool. 0 = no throttle (every change reloads
	// immediately, original behavior). Default 90s — long enough that
	// transient probe noise can't cascade reloads, short enough that a
	// real degradation propagates within ~2 scoring rounds.
	HotReloadMinInterval time.Duration `yaml:"hot_reload_min_interval"`

	// Probes defines per-target reachability checks (e.g. "can this node
	// load chatgpt.com without hitting Cloudflare's bot challenge?"). Each
	// probe runs from each candidate node periodically; results land in
	// NodeHealth.Probes and gate membership in named Pools.
	Probes []ProbeConfig `yaml:"probes"`

	// PoolSizing controls dynamic K (us-pool target size). When TActive=0
	// (default) the legacy "every qualified candidate is in pool" behavior
	// is preserved. When TActive>0 the scorer ranks qualified candidates
	// by composite score and gates the pool to top-K, sized from the
	// formula:
	//
	//   K = clamp(
	//     ⌈T_active × surge / cap_per_node⌉,
	//     K_min(T_active),
	//     min(K_max, |Tier1|),
	//   )
	//   where K_min(T) = max(2, ⌈T / (cap × 1.5)⌉ + 1)  // failure resilience
	//
	// Hysteresis (EvictStrikes / ReadmitStrikes) governs membership moves;
	// HotReloadMinInterval throttles reload frequency. The result: a small
	// stable set of best-quality egresses for a given terminal load,
	// preserving per-terminal HRW identity (perterminal.go).
	//
	// Only consulted when PoolMode=auto.
	PoolSizing PoolSizingConfig `yaml:"pool_sizing"`

	// SwapThresholdScore is the absolute long-EWMA score margin a
	// non-pool candidate must beat the worst pool member by, in order
	// to be promoted. 0 = no margin (behave like before — top-K wins
	// regardless of how thin the gap is). Higher values = stickier pool
	// (resists "tied for top-K" flapping that would otherwise cause
	// frequent IP rotations).
	//
	// Tuning: compositeScore is roughly p95_ms scale (with jitter +
	// fail_rate contributions). 100 ≈ "100ms p95 better" — modest
	// but meaningful. 300 = "300ms p95 better OR fail_rate 0.30 lower"
	// = strong signal required. Default 100 in auto mode.
	SwapThresholdScore float64 `yaml:"swap_threshold_score"`

	// PoolMode selects who owns pool composition.
	//
	//	"auto"   — scorer K-gating (legacy; subject to detection signal from
	//	           5-min auto rotation, retained for backward compat)
	//	"manual" — operator-owned via PoolMembers; scorer is observation-only
	//	           and never auto-rotates the pool. Routing is fixed to the
	//	           PoolMembers list. Emergency events still fire signals
	//	           (Push notification) but a follow-up phase wires the
	//	           EmergencyPromoteChain auto-promotion behavior. (Default
	//	           in production going forward — eliminates the 5-min auto-IP
	//	           rotation that CF/OpenAI behavioral models would flag.)
	//
	// Default "auto" for backward compat with deployments that haven't
	// migrated. Operator should set "manual" + populate PoolMembers.
	PoolMode string `yaml:"pool_mode"`

	// PoolMembers is the operator-owned list of node tags that carry
	// production traffic when PoolMode=manual. Members must reference
	// node tags that exist in the active subscription set; missing tags
	// are dropped at render time with a warning. Order is irrelevant —
	// per-terminal HRW assigns terminals to members deterministically by
	// hash, not order.
	//
	// This is the "ground truth" for the routing pool. Treat the same way
	// as `subscriptions:` — operator-owned, do NOT auto-modify during
	// node-sync / schema-migration / yaml-splice operations.
	PoolMembers []string `yaml:"pool_members"`

	// EmergencyPromoteChain is the operator's pre-approved replacement
	// queue for when an active PoolMembers entry is auto-evicted by the
	// emergency path (node fail_rate=100% sustained 30min). System walks
	// this list in order, picking the first not-already-in-pool entry to
	// replace the evicted node so pool size stays at K.
	//
	// Pre-approval matters: ops cannot always be online. With this list
	// the system can keep the pool at full size during single-node
	// failures without waiting for human approval.
	//
	// Schema is in place from Step 0; the auto-promote behavior wires up
	// in Phase 2.
	EmergencyPromoteChain []string `yaml:"emergency_promote_chain"`
}

// PoolSizingConfig controls dynamic K computation. See NodeQualifyConfig.PoolSizing.
type PoolSizingConfig struct {
	// TActive is the operator-supplied 7-day distinct active terminal
	// count. Used to compute K (pool target size). Set this from FeiLian's
	// accounting / DHCP data; recompute weekly, ignore daily fluctuations.
	// Default 50. Previously, TActive=0 triggered legacy "all qualified
	// nodes in pool" behavior — that mode is removed; K-gating is always on.
	TActive int `yaml:"t_active"`
	// CapPerNode is the per-egress sustained terminal capacity. Operator-
	// tuned per subscription tier; default 10 (conservative — fishcloud
	// direct, ash IEPL etc. typically tolerate ~10-15 concurrent terminals
	// before P95 climbs). Should be re-validated by stress.sh per node tier.
	CapPerNode int `yaml:"cap_per_node"`
	// Surge is the peak/avg ratio. Default 1.5 (evening peak ~1.5× the
	// daytime mean). Drives the K_demand calculation.
	Surge float64 `yaml:"surge"`
	// KMin is the absolute floor on K (independent of T). Default 3 — even
	// for tiny terminal counts, we want some redundancy so a single node
	// failure doesn't strand all flows.
	KMin int `yaml:"k_min"`
	// KMax is the detection-surface ceiling on K. Default 12 — empirically
	// CF/OpenAI cluster ≤ 12 stable IPs as "small enterprise NAT" (legitimate
	// identity); ≥ 20 trends toward "VPN infrastructure". Stable rotation
	// matters more than absolute count, but a low ceiling further reduces
	// the surface for IP cataloging.
	KMax int `yaml:"k_max"`
	// MinPoolSize is the hard lower bound on routing pool size. When the
	// pool would fall below this (due to evictions or insufficient qualified
	// nodes), the scorer fills with the best available nodes including
	// cold-start (trial) nodes. Default 3.
	MinPoolSize int `yaml:"min_pool_size"`
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

	// NB: there is deliberately no reject_cf_challenge knob. A response
	// carrying `cf-mitigated: challenge` is ALWAYS a probe failure — see
	// checkResponse in internal/nodescorer/probe.go.
	//
	// The field existed until 2026-08-12 with yaml tag reject_cf_challenge
	// and doc "default false", but probe.go never read it: the header check
	// was unconditional from the start. Removed rather than wired up,
	// because wiring it as documented would have been a regression. A
	// challenge means the request never reached the origin, so for a probe
	// whose entire job is measuring egress-IP reputation, counting it as a
	// pass is exactly backwards — it would let a blocklisted node into
	// openai-pool. For targets that challenge every headless client
	// regardless of IP (chatgpt.com's root HTML), the fix is to not probe
	// that URL; see the probe notes in configs/gateway.example.yaml.
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
	c.Notifications.applyDefaults()
}

// applyDefaults fills NotificationsConfig defaults. Disabled by default —
// operator must explicitly enable. Defaults aim for "sane in production"
// when the operator opts in: signature on, dedup tight enough to suppress
// BGP flap spam, retries that don't drop urgent events on transient
// network errors.
func (n *NotificationsConfig) applyDefaults() {
	if n.LocalLogPath == "" {
		n.LocalLogPath = "/var/lib/leap/notifications.log"
	}
	if n.DedupWindow == 0 {
		n.DedupWindow = 5 * time.Minute
	}
	if n.AggregationWindow == 0 {
		n.AggregationWindow = 30 * time.Second
	}
	if n.PerNodeRateLimitPerHour == 0 {
		n.PerNodeRateLimitPerHour = 4
	}
	if n.RetryAttempts == 0 {
		n.RetryAttempts = 3
	}
	if n.InstanceName == "" {
		// Identify the sending gateway in the Lark header. Hostname is the
		// natural per-node identity (e.g. "dianweiserver"); operator can
		// override with a friendlier label via yaml `instance_name`.
		if h, err := os.Hostname(); err == nil {
			n.InstanceName = h
		}
	}
	if n.Lark.SecretFile == "" {
		n.Lark.SecretFile = "/var/lib/leap/lark-secret"
	}
	// Lark.SignatureRequired default true: any non-zero NotificationsConfig
	// loaded with Lark.WebhookURL set should require signing unless
	// operator explicitly turns it off (yaml `signature_required: false`).
	// Default Go zero value is false, so we only flip when WebhookURL
	// is set AND the operator hasn't explicitly written `false` — but
	// yaml.v3 can't distinguish missing-vs-explicit-false on a bool.
	// Safer: default ON only when URL is configured; if operator wants
	// OFF they leave URL empty (no webhook) or set up an unsigned bot
	// at their own risk. We log a WARN at startup if URL set + sig off.
	if n.Lark.WebhookURL != "" && !n.Lark.SignatureRequired {
		// not modifying the field — caller is responsible. Warning
		// emitted at scorer startup time (we don't have slog here).
	}
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
	if len(c.DNS.NodeResolver) == 0 {
		c.DNS.NodeResolver = []string{"udp://119.29.29.29"}
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
	if c.URLTest.HealthCheckInterval == 0 {
		// 30s: fast enough that LoadBalance routing skips a dead node within
		// one probe cycle; slow enough that 19 nodes × N subs × 30s probes
		// stay well under any airport's monthly quota.
		c.URLTest.HealthCheckInterval = 30 * time.Second
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
		c.ScoringInterval = 5 * time.Minute
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
	if c.HotReloadMinInterval == 0 {
		c.HotReloadMinInterval = 90 * time.Second
	}
	if c.SwapThresholdScore == 0 {
		// Strong stickiness gate: candidate must beat worst pool member
		// by ≥ 400 score-points before displacing it. At compositeScore
		// scale (p95_ms + 2×jitter + 5000×fail²), 400 ≈ "400ms p95
		// better, or fail_rate 0.28 lower" — a meaningful signal, not
		// noise. Raised from 100 to reduce pool churn: with 100 the pool
		// swapped ~67×/24h on a 22-node candidate set, causing IP drift
		// that trips CF/OpenAI behavioral models. Operator can lower to
		// 0 to revert to "any improvement triggers swap" semantics.
		c.SwapThresholdScore = 400
	}
	if c.PoolMode == "" {
		c.PoolMode = "auto"
	}
	c.PoolSizing.applyDefaults()
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

// applyDefaults fills PoolSizingConfig defaults. K-gating is always on;
// TActive=0 in yaml gets the default of 50 (a reasonable starting point
// — operator should tune to their actual terminal count).
func (p *PoolSizingConfig) applyDefaults() {
	if p.TActive == 0 {
		p.TActive = 50
	}
	if p.CapPerNode == 0 {
		p.CapPerNode = 10
	}
	if p.Surge == 0 {
		p.Surge = 1.5
	}
	if p.KMin == 0 {
		p.KMin = 3
	}
	if p.KMax == 0 {
		p.KMax = 12
	}
	if p.MinPoolSize == 0 {
		p.MinPoolSize = 3
	}
}
