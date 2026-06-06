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
func (r *Renderer) buildRules() []string {
	var rules []string

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

	// Pool rule_sets route to their named pool. In overseas mode these are
	// the ONLY non-default routing rules (everything else → MATCH,out); in
	// whitelist mode they sit before the whitelist block so a pooled tag
	// (e.g. geosite-openai) goes to openai-pool, not the generic `out`.
	// Emitted in both modes because a pool's reason for existing is to
	// segregate its sites regardless of overall route mode.
	poolRouted := map[string]bool{}
	for _, tag := range r.poolRuleSets() {
		poolName := r.poolForRuleSet(tag)
		if poolName == "" {
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
				continue // already routed to a pool above
			}
			rules = append(rules, "RULE-SET,"+tag+","+outSelector)
		}
		for _, sx := range r.cfg.Route.Whitelist.DomainSuffix {
			rules = append(rules, "DOMAIN-SUFFIX,"+sx+","+outSelector)
		}
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
		rules = append(rules, "MATCH,"+outSelector)
	}

	return rules
}
