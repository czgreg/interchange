// rules.go — rules + rule-providers.
//
// mihomo natively reads sing-box .srs files (format: srs). leap-gateway
// already manages those .srs files on disk under cfg.RuleSetsDir, so we
// reference them directly via type=file rule-providers — no second copy,
// same files sing-box would load.

package mihomo

import (
	"path/filepath"
)

const (
	rsGeositeCN = "geosite-cn"
	rsGeoipCN   = "geoip-cn"
)

// buildRuleProviders emits `rule-providers:` — one provider per rule-set tag
// that the route table refers to. CN infra (geosite-cn / geoip-cn) always
// emitted; whitelist tags only emitted in whitelist mode.
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

	if r.cfg.Route.Mode == "whitelist" {
		for _, tag := range r.cfg.Route.Whitelist.Geosites {
			rules = append(rules, "RULE-SET,"+tag+","+outSelector)
		}
		for _, sx := range r.cfg.Route.Whitelist.DomainSuffix {
			rules = append(rules, "DOMAIN-SUFFIX,"+sx+","+outSelector)
		}
		for _, tag := range r.cfg.Route.Whitelist.Geoips {
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
