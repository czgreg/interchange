// Package mihomo renders a complete mihomo (clash.meta) Clash YAML config
// from leap-gateway's parsed subscription outbounds + node config + whitelist.
// It mirrors internal/singbox/renderer.go's high-level behavior but produces
// a different proxy engine's config.
//
// Key architectural difference from the sing-box renderer:
//
//	sing-box: out (selector) → urltest-primary → [first sub's nodes]
//	                        → urltest-backup  → [other subs' nodes]   (dormant)
//	                        → direct
//	          single-active. backup pool only used during primary failure.
//
//	mihomo:   out (selector) → us-pool (load-balance, consistent-hashing)
//	                        → pin    (manual selector for ops override)
//	                        → DIRECT
//	          us-pool MEMBER LIST = ALL enabled subs' nodes filtered by
//	          NodePattern. consistent-hashing: same destination → same node
//	          (sticky), different destinations → spread across nodes. All
//	          nodes are simultaneously active under different flows.
//
// Config schema mappings (sing-box → mihomo):
//
//	dns.servers / dns.rules     → dns: { nameserver, fallback, ... } block
//	inbounds[type=tun]          → tun: { enable, device, stack, ... } block
//	inbounds[type=http loopback]→ mixed-port + ports: { http: ... }
//	outbounds                   → proxies: + proxy-groups:
//	route.rule_set              → rule-providers: + rules: with RULE-SET,...
//	experimental.clash_api      → external-controller + secret
package mihomo

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/leap-gateway/leap-gateway/internal/config"
	"github.com/leap-gateway/leap-gateway/internal/subscribe"
	"gopkg.in/yaml.v3"
)

// Loopback-only HTTP proxy ports (kept stable across sing-box and mihomo
// renderers so leap-gateway control-plane code that targets these ports
// doesn't change). Note: in mihomo we use a single mixed-port + a
// per-pool selector approach instead of three separate HTTP inbounds, so
// only the primary one is preserved as the "leap-internal" proxy port.
const (
	// LeapInternalProxyPort: the loopback HTTP proxy that rulesets.Manager
	// uses to fetch .srs blobs through the airport. Equivalent to sing-box
	// renderer's LeapInternalProxyPort (11080).
	LeapInternalProxyPort = 11080
)

// Renderer turns parsed subscription outbounds + node config into a complete
// mihomo Clash YAML config. The public API mirrors internal/singbox.Renderer
// so the control plane can swap engines via a single config switch.
type Renderer struct {
	cfg  config.DataPlaneConfig // reused: schema is engine-agnostic
	node config.NodeConfig
	subs []config.SubscriptionEntry
	// qualifiedOverride, when non-nil, limits us-pool members to the given
	// set of node tags (used by nodescorer during hot-reload). nil = use all
	// NodePattern-matched nodes (normal rendering path).
	qualifiedOverride []string
	// pools are the named select/load-balance groups rendered alongside
	// us-pool (e.g. openai-pool). Their rule_sets route to them instead of
	// the default `out` selector.
	pools []config.PoolConfig
	// poolMembers maps pool name → member node tags, set by nodescorer at
	// hot-reload time (members = us-pool-qualified ∩ passing the pool's
	// required probes). nil/absent for a pool name → that pool falls back
	// to the full us-pool member set (so it works before the first probe
	// round completes).
	poolMembers map[string][]string
	// probeListener toggles emission of the leap-probe HTTP listener +
	// probe-out selector group used by nodescorer's per-node site probes.
	probeListener bool
	// perTerminal, when true, pins each client terminal's whitelisted
	// traffic to one egress node via SRC-IP-CIDR slices under a `perterm`
	// sub-rule. Requires data_plane.tproxy_port (TPROXY preserves real srcIP).
	perTerminal bool
}

// WithLoadBalance sets the load-balance behavior. perTerminal=true emits
// the per-terminal SRC-IP-CIDR pinning (see config.LoadBalanceConfig).
func (r *Renderer) WithLoadBalance(perTerminal bool) *Renderer {
	r.perTerminal = perTerminal
	return r
}

// NewRenderer constructs a Renderer with the given engine-agnostic config.
// Node + subscriptions are attached later via WithNode / WithSubscriptions.
func NewRenderer(cfg config.DataPlaneConfig) *Renderer {
	return &Renderer{cfg: cfg}
}

// WithNode attaches the FeiLian forwarding-node info (CIDR, tun0 gw, egress
// iface). Required for production rendering — without it we omit the TUN
// inbound and the rendered config is only useful for static validation.
func (r *Renderer) WithNode(n config.NodeConfig) *Renderer {
	r.node = n
	return r
}

// WithSubscriptions captures the configured subscription order so render
// decisions that depend on it (e.g. fallback-priority subs) can use it.
func (r *Renderer) WithSubscriptions(subs []config.SubscriptionEntry) *Renderer {
	r.subs = append([]config.SubscriptionEntry(nil), subs...)
	return r
}

// WithPools attaches the named-pool config (openai-pool etc.). Enables the
// probe listener automatically when any pool declares requires_passing —
// the probes those pools gate on need a way to run.
func (r *Renderer) WithPools(pools []config.PoolConfig) *Renderer {
	r.pools = append([]config.PoolConfig(nil), pools...)
	for _, p := range pools {
		if len(p.RequiresPassing) > 0 {
			r.probeListener = true
			break
		}
	}
	return r
}

// SetSubscriptions replaces the subscription order in place. Used by API
// handlers that mutate the config and need a re-render with the new state.
func (r *Renderer) SetSubscriptions(subs []config.SubscriptionEntry) {
	r.subs = append([]config.SubscriptionEntry(nil), subs...)
}

// SetWhitelist re-seats the renderer's route mode + whitelist snapshot.
func (r *Renderer) SetWhitelist(mode string, wl config.WhitelistConfig) {
	r.cfg.Route.Mode = mode
	r.cfg.Route.Whitelist = wl
}

// SetFakeIPSkip re-seats the DNS fake-ip-filter skip-list. Called by the
// API layer after PUT /api/whitelist so the updated fake-ip-filter is
// emitted in the next Write without needing a full config reload.
func (r *Renderer) SetFakeIPSkip(suffixes []string) {
	r.cfg.DNS.FakeIPSkipSuffixes = append([]string(nil), suffixes...)
}

// RenderWithQualifiedNodes produces a Clash YAML where us-pool contains
// only the given qualified node tags. Used by nodescorer to hot-reload
// mihomo with an updated pool after a scoring round. nil = use all
// NodePattern-matched nodes (normal rendering path).
func (r *Renderer) RenderWithQualifiedNodes(outbounds []subscribe.Outbound, qualified []string) ([]byte, error) {
	clone := *r
	clone.qualifiedOverride = qualified
	return clone.Write(outbounds)
}

// RenderWithPools is the nodescorer hot-reload entry that also assigns
// named-pool memberships. usPool is the qualified us-pool set; poolMembers
// maps each named pool → its member tags (us-pool ∩ passing that pool's
// probes). A pool absent from poolMembers falls back to the full us-pool.
func (r *Renderer) RenderWithPools(outbounds []subscribe.Outbound, usPool []string, poolMembers map[string][]string) ([]byte, error) {
	clone := *r
	clone.qualifiedOverride = usPool
	clone.poolMembers = poolMembers
	return clone.Write(outbounds)
}

// Path returns the on-disk path the rendered config should be written to.
// For mihomo this is typically /etc/leap/mihomo/config.yaml.
func (r *Renderer) Path() string { return r.cfg.ConfigPath }

// RenderOnly renders the config and returns the bytes WITHOUT writing to disk.
// Safe to call when you need the rendered YAML for inspection or temp-path
// validation without touching the production config.
func (r *Renderer) RenderOnly(outbounds []subscribe.Outbound) ([]byte, error) {
	doc := r.build(outbounds)
	return yaml.Marshal(doc)
}

// Write renders a complete mihomo Clash YAML config and writes it atomically
// to r.cfg.ConfigPath. Returns the rendered bytes for inspection.
//
// Atomic rename through a .tmp sibling so a render mid-flight never leaves
// mihomo with a half-written config.
func (r *Renderer) Write(outbounds []subscribe.Outbound) ([]byte, error) {
	out, err := r.RenderOnly(outbounds)
	if err != nil {
		return nil, fmt.Errorf("mihomo render: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(r.cfg.ConfigPath), 0o755); err != nil {
		return nil, fmt.Errorf("mihomo render: mkdir: %w", err)
	}
	tmp := r.cfg.ConfigPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return nil, fmt.Errorf("mihomo render: write tmp: %w", err)
	}
	if err := os.Rename(tmp, r.cfg.ConfigPath); err != nil {
		return nil, fmt.Errorf("mihomo render: rename: %w", err)
	}
	return out, nil
}

// build is the orchestrator. Each section maps to a top-level Clash YAML key.
// Order matters for human readability of the generated file, not for parser
// behavior — yaml.v3 preserves the map insertion order via yaml.Node when we
// marshal a yaml.Node directly. Plain map[string]any does not preserve order;
// for stable output we'd need yaml.Node, but mihomo accepts any order.
func (r *Renderer) build(outbounds []subscribe.Outbound) map[string]any {
	doc := map[string]any{
		"mode":                "rule",
		"log-level":           defaultIfEmpty(r.cfg.LogLevel, "info"),
		"ipv6":                false,
		"unified-delay":       true,
		"geodata-mode":        false,
		"geo-auto-update":     false,
		"external-controller": r.cfg.ClashAPI.ExternalController,
		"mixed-port":          LeapInternalProxyPort,
	}
	if r.cfg.ClashAPI.Secret != "" {
		doc["secret"] = r.cfg.ClashAPI.Secret
	}

	doc["dns"] = r.buildDNS()
	doc["tun"] = r.buildTUN()
	doc["sniffer"] = r.buildSniffer()
	// TPROXY port: when set, mihomo accepts transparent-proxy connections that
	// the kernel routes via iptables TPROXY + fwmark. This preserves the
	// real client sourceIP (10.8.x.x) unlike TUN mode, enabling per-terminal
	// source-IP-based routing. TUN is kept for DNS hijacking (any:53).
	if r.cfg.TProxyPort > 0 {
		doc["tproxy-port"] = r.cfg.TProxyPort
	}
	doc["proxies"] = r.buildProxies(outbounds)
	doc["proxy-groups"] = r.buildProxyGroups(outbounds)
	doc["rule-providers"] = r.buildRuleProviders()
	rules, subRules := r.buildRules(outbounds)
	doc["rules"] = rules
	if len(subRules) > 0 {
		doc["sub-rules"] = subRules
	}
	// Probe listener: a loopback HTTP inbound pinned (via `proxy:`) to the
	// probe-out selector. nodescorer flips probe-out to each node, then
	// GETs site URLs through this port to read real status + headers
	// (cf-mitigated detection). Only emitted when a pool gates on probes.
	if r.probeListener {
		doc["listeners"] = r.buildListeners()
	}
	// Persist DNS cache across hot-reloads and restarts. store-fake-ip keeps
	// fakeip mappings alive so employees don't pay the cold DNS round-trip
	// (~400ms cross-border DoH) after every nodescorer hot-reload.
	doc["experimental"] = map[string]any{
		"cache-file": map[string]any{
			"enable":        true,
			"path":          "./cache.db",
			"store-fake-ip": true,
		},
	}

	return doc
}

func defaultIfEmpty(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
