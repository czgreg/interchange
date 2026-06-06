// rules.go — rules + rule-providers.
//
// mihomo natively reads sing-box .srs files (format: srs). leap-gateway
// already manages those .srs files on disk under cfg.RuleSetsDir, so we
// reference them directly via type=file rule-providers — no second copy,
// same files sing-box would load.

package mihomo

import (
	"path/filepath"
	"strings"

	"github.com/leap-gateway/leap-gateway/internal/subscribe"
)

const (
	rsGeositeCN = "geosite-cn"
	rsGeoipCN   = "geoip-cn"
)

// poolForRuleSet returns the name of the pool a given rule-set tag routes
// to, or "" if the tag isn't claimed by any pool (→ routes to the default
// `out` selector). First pool wins if somehow two claim the same tag.
func (r *Renderer) poolForRuleSet(tag string) string {
	for _, p := range r.pools {
		for _, rs := range p.RuleSets {
			if rs == tag {
				return p.Name
			}
		}
	}
	return ""
}

// poolRuleSets returns every rule-set tag claimed by any pool, deduped.
// Used by buildRuleProviders to ensure those tags get a provider even when
// they aren't in the whitelist (overseas mode, or whitelist that doesn't
// list them explicitly).
func (r *Renderer) poolRuleSets() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range r.pools {
		for _, rs := range p.RuleSets {
			if !seen[rs] {
				seen[rs] = true
				out = append(out, rs)
			}
		}
	}
	return out
}

// buildRuleProviders emits `rule-providers:` — one provider per rule-set tag
// that the route table refers to. CN infra (geosite-cn / geoip-cn) always
// emitted; whitelist tags only emitted in whitelist mode; pool rule_sets
// always emitted (they route regardless of mode).
//
// Format: mihomo accepts `mrs` (its own binary format) — NOT sing-box's
// `srs`. MetaCubeX publishes both side by side; the .mrs file lives next
// to the .srs one with the same stem. We expect the deploy chain to fetch
// both into RuleSetsDir as <tag>.mrs.
func (r *Renderer) buildRuleProviders() map[string]any {
	dir := r.cfg.RuleSetsDir

	rp := map[string]any{
		rsGeositeCN: ruleProvider("domain", filepath.Join(dir, rsGeositeCN+".mrs")),
		rsGeoipCN:   ruleProvider("ipcidr", filepath.Join(dir, rsGeoipCN+".mrs")),
	}

	if r.cfg.Route.Mode == "whitelist" {
		for _, tag := range r.cfg.Route.Whitelist.Geosites {
			rp[tag] = ruleProvider("domain", filepath.Join(dir, tag+".mrs"))
		}
		for _, tag := range r.cfg.Route.Whitelist.Geoips {
			rp[tag] = ruleProvider("ipcidr", filepath.Join(dir, tag+".mrs"))
		}
	}
	// Pool rule_sets — always present. behavior inferred from tag prefix
	// (geoip-* → ipcidr, everything else → domain).
	for _, tag := range r.poolRuleSets() {
		if _, ok := rp[tag]; ok {
			continue
		}
		behavior := "domain"
		if strings.HasPrefix(tag, "geoip-") {
			behavior = "ipcidr"
		}
		rp[tag] = ruleProvider(behavior, filepath.Join(dir, tag+".mrs"))
	}
	return rp
}

func ruleProvider(behavior, path string) map[string]any {
	return map[string]any{
		"type":     "file",
		"format":   "mrs",
		"behavior": behavior,
		"path":     path,
	}
}

// buildRules emits the `rules:` array. Order matters — first match wins.
//
// overseas mode (default):
//
//	IP-CIDR,LAN,DIRECT (no-resolve)
//	GEOIP/RULE-SET ip_is_private DIRECT
//	RULE-SET,geosite-cn,DIRECT
//	RULE-SET,geoip-cn,DIRECT,no-resolve
//	MATCH,out
//
// whitelist mode:
//
//	IP-CIDR,LAN,DIRECT (no-resolve)
//	RULE-SET,geosite-cn,DIRECT
//	RULE-SET,geoip-cn,DIRECT,no-resolve
//	[whitelisted geosite tags] → out
//	[whitelisted DOMAIN-SUFFIX] → out
//	[whitelisted geoip tags] → out (no-resolve)
//	[whitelisted IP-CIDR]   → out (no-resolve)
//	MATCH,DIRECT
//
// per-terminal mode (load_balance.per_terminal=true): domain-matched
// whitelist entries dispatch to the `perterm` sub-rule instead of `out`.
// perterm holds SRC-IP-CIDR,<terminal/32>,<node> slices (HRW-assigned over
// the us-pool members) + a MATCH,us-pool fallback — so one terminal's
// whitelisted traffic all exits one node. IP-based whitelist (geoip /
// ip_cidr, no-resolve) still goes to `out` (hardcoded-IP apps don't drift,
// and no-resolve doesn't translate into a SUB-RULE condition).
//
// Returns (rules, subRules). subRules is empty unless per-terminal is on.
func (r *Renderer) buildRules(outbounds []subscribe.Outbound) ([]string, map[string]any) {
	var rules []string
	subRules := map[string]any{}

	// Private/LAN — no DNS resolution needed.
	rules = append(rules,
		"IP-CIDR,127.0.0.0/8,DIRECT,no-resolve",
		"IP-CIDR,10.0.0.0/8,DIRECT,no-resolve",
		"IP-CIDR,172.16.0.0/12,DIRECT,no-resolve",
		"IP-CIDR,192.168.0.0/16,DIRECT,no-resolve",
		"IP-CIDR,169.254.0.0/16,DIRECT,no-resolve",
		"IP-CIDR,224.0.0.0/4,DIRECT,no-resolve",
	)

	// CN direct — must come before whitelist hits so CN endpoints of
	// global services (e.g. apple.com.cn) don't get routed via airport.
	rules = append(rules,
		"RULE-SET,"+rsGeositeCN+",DIRECT",
		"RULE-SET,"+rsGeoipCN+",DIRECT,no-resolve",
	)

	// Per-terminal: build the perterm sub-rule (SRC-IP-CIDR slices) once.
	// domainTarget below dispatches domain whitelist hits into it.
	perterm := r.perTerminal && r.node.ClientSubnet != ""
	if perterm {
		slices := r.perTerminalSlices(outbounds)
		if len(slices) == 0 {
			// No us-pool members to slice across — disable perterm this
			// render rather than emit a sub-rule that dead-ends.
			perterm = false
		} else {
			subRules["perterm"] = slices
		}
		// Also build per-pool perterm sub-rules for probe-gated pools.
		// A probe-gated pool (e.g. openai-pool) should only slice across
		// its own qualified members — routing openai traffic through the
		// full us-pool would expose clients to nodes that failed the probe.
		for _, p := range r.pools {
			if len(p.RequiresPassing) == 0 {
				continue // not probe-gated; generic perterm is fine
			}
			subName := "perterm-" + p.Name
			poolSlices := r.perTerminalSlicesForPool(p.Name, outbounds)
			if len(poolSlices) > 0 {
				subRules[subName] = poolSlices
			}
		}
	}

	// domainRule routes a domain-matching condition to either the perterm
	// sub-rule (per-terminal) or the `out` selector (default).
	domainRule := func(cond string) string {
		if perterm {
			return "SUB-RULE,(" + cond + "),perterm"
		}
		return cond + "," + outSelector
	}

	// Pool rule_sets (openai-pool etc.). When per-terminal is ON, domain pool
	// tags route through a pool-specific perterm sub-rule (perterm-<poolname>)
	// so only probe-passing nodes serve that pool's traffic. Pools without
	// requires_passing fall back to the generic perterm (us-pool members).
	poolRouted := map[string]bool{}
	for _, tag := range r.poolRuleSets() {
		poolName := r.poolForRuleSet(tag)
		if poolName == "" {
			continue
		}
		if perterm && !strings.HasPrefix(tag, "geoip-") {
			// Route through pool-specific perterm if it was generated,
			// else fall back to generic perterm.
			subName := "perterm-" + poolName
			if _, ok := subRules[subName]; ok {
				rules = append(rules, "SUB-RULE,(RULE-SET,"+tag+"),"+subName)
			} else {
				rules = append(rules, domainRule("RULE-SET,"+tag))
			}
			poolRouted[tag] = true
			continue
		}
		noResolve := ""
		if strings.HasPrefix(tag, "geoip-") {
			noResolve = ",no-resolve"
		}
		rules = append(rules, "RULE-SET,"+tag+","+poolName+noResolve)
		poolRouted[tag] = true
	}

	if r.cfg.Route.Mode == "whitelist" {
		for _, tag := range r.cfg.Route.Whitelist.Geosites {
			if poolRouted[tag] {
				continue
			}
			rules = append(rules, domainRule("RULE-SET,"+tag))
		}
		for _, sx := range r.cfg.Route.Whitelist.DomainSuffix {
			rules = append(rules, domainRule("DOMAIN-SUFFIX,"+sx))
		}
		// IP-based whitelist stays on `out` (no-resolve; hardcoded-IP apps
		// like Telegram MTProto don't suffer subdomain drift).
		for _, tag := range r.cfg.Route.Whitelist.Geoips {
			if poolRouted[tag] {
				continue
			}
			rules = append(rules, "RULE-SET,"+tag+","+outSelector+",no-resolve")
		}
		for _, cidr := range r.cfg.Route.Whitelist.IPCIDR {
			rules = append(rules, "IP-CIDR,"+cidr+","+outSelector+",no-resolve")
		}
		rules = append(rules, "MATCH,DIRECT")
	} else {
		// overseas mode: everything non-CN goes overseas. Per-terminal
		// dispatches the catch-all through perterm; else straight to out.
		if perterm {
			rules = append(rules, "SUB-RULE,(MATCH),perterm")
		} else {
			rules = append(rules, "MATCH,"+outSelector)
		}
	}

	return rules, subRules
}
