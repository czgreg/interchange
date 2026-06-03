package singbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/leap-gateway/leap-gateway/internal/config"
	"github.com/leap-gateway/leap-gateway/internal/subscribe"
)

func newTestRenderer(t *testing.T, withNode bool) (*Renderer, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.SingBoxConfig{
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	cfg.ApplyDefaults()
	r := NewRenderer(cfg)
	if withNode {
		r = r.WithNode(config.NodeConfig{
			ClientSubnet:  "10.8.11.0/24",
			Tun0GatewayIP: "10.8.11.1",
			EgressIface:   "ens18",
		})
		// Production deployments enable TUN; ApplyDefaults didn't because that
		// flag is opt-in.
		r.cfg.TUN.Enabled = true
	}
	return r, cfg.ConfigPath
}

func TestRendererProductionMode(t *testing.T) {
	r, _ := newTestRenderer(t, true)
	outs := []subscribe.Outbound{
		{"type": "vmess", "tag": "n1", "server": "a", "server_port": 443, "uuid": "x"},
		{"type": "trojan", "tag": "n2", "server": "b", "server_port": 443, "password": "p"},
	}
	data, err := r.Write(outs)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("output is not valid json: %v", err)
	}

	// DNS section: servers + rules + fakeip + final.
	dns, ok := doc["dns"].(map[string]any)
	if !ok {
		t.Fatalf("missing dns section")
	}
	servers, _ := dns["servers"].([]any)
	wantServers := map[string]bool{"remote": false, "local": false, "fakeip": false, "local-bootstrap": false}
	for _, s := range servers {
		m, _ := s.(map[string]any)
		tag, _ := m["tag"].(string)
		if _, ok := wantServers[tag]; ok {
			wantServers[tag] = true
		}
	}
	for tag, found := range wantServers {
		if !found {
			t.Errorf("dns.servers missing tag=%q", tag)
		}
	}
	if fakeip, _ := dns["fakeip"].(map[string]any); fakeip == nil {
		t.Errorf("dns.fakeip missing")
	}
	if dns["final"] != "remote" {
		t.Errorf("dns.final = %v, want remote", dns["final"])
	}

	// Inbounds (single-sub case): tun + dns + leap-internal-http (rulesets
	// fetcher) + leap-internal-http-primary (egress probe pinned to
	// urltest-primary). No backup inbound because we have only one
	// subscription. No socks/http because we didn't set those.
	inbounds, _ := doc["inbounds"].([]any)
	if len(inbounds) != 4 {
		t.Errorf("want 4 inbounds (tun, dns, leap-internal-http, leap-internal-http-primary), got %d", len(inbounds))
	}
	tags := make(map[string]bool)
	for _, ib := range inbounds {
		m, _ := ib.(map[string]any)
		tags[m["tag"].(string)] = true
	}
	for _, want := range []string{"tun-in", "dns-in", "leap-internal-http-in", "leap-internal-http-primary-in"} {
		if !tags[want] {
			t.Errorf("missing inbound %q: have %v", want, tags)
		}
	}
	if tags["leap-internal-http-backup-in"] {
		t.Errorf("backup inbound should not be present in single-sub case")
	}

	// Outbounds: out + urltest + n1 + n2 + direct + dns-out + block = 7.
	allOuts, _ := doc["outbounds"].([]any)
	if len(allOuts) != 7 {
		t.Errorf("want 7 outbounds, got %d", len(allOuts))
	}
	gotTags := map[string]string{}
	for _, ob := range allOuts {
		m, _ := ob.(map[string]any)
		gotTags[m["tag"].(string)] = m["type"].(string)
	}
	for _, want := range []string{"out", "urltest-primary", "n1", "n2", "direct", "dns-out", "block"} {
		if _, ok := gotTags[want]; !ok {
			t.Errorf("outbounds missing tag=%q", want)
		}
	}
	if gotTags["out"] != "selector" {
		t.Errorf("out is %q, want selector", gotTags["out"])
	}
	if gotTags["dns-out"] != "dns" {
		t.Errorf("dns-out type is %q, want dns", gotTags["dns-out"])
	}

	// Route: rule_set + rules + final=out.
	route, _ := doc["route"].(map[string]any)
	if route == nil {
		t.Fatalf("missing route section")
	}
	ruleSet, _ := route["rule_set"].([]any)
	if len(ruleSet) != 2 {
		t.Errorf("want 2 rule_set entries, got %d", len(ruleSet))
	}
	rules, _ := route["rules"].([]any)
	if len(rules) < 3 {
		t.Errorf("want at least 3 route rules, got %d", len(rules))
	}
	if route["final"] != "out" {
		t.Errorf("route.final = %v, want out", route["final"])
	}

	// experimental.clash_api + cache_file.
	exp, _ := doc["experimental"].(map[string]any)
	if exp == nil {
		t.Fatalf("missing experimental section")
	}
	if _, ok := exp["clash_api"]; !ok {
		t.Errorf("missing clash_api")
	}
	if _, ok := exp["cache_file"]; !ok {
		t.Errorf("missing cache_file")
	}
}

func TestRendererLabMode(t *testing.T) {
	// Lab mode: no NodeConfig, but with explicit SOCKSListen/HTTPListen for
	// cmd/selftest's static-validation use case. Output should still be
	// schema-valid and pass `sing-box check`.
	r, _ := newTestRenderer(t, false)
	r.cfg.SOCKSListen = "0.0.0.0:1080"
	r.cfg.HTTPListen = "0.0.0.0:1081"

	data, err := r.Write([]subscribe.Outbound{
		{"type": "vmess", "tag": "n1", "server": "a", "server_port": 443, "uuid": "x"},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	var doc map[string]any
	_ = json.Unmarshal(data, &doc)

	// Lab mode → no tun-in / dns-in, just socks + http + leap-internal-http
	// + leap-internal-http-primary (the latter two are always-on).
	inbounds, _ := doc["inbounds"].([]any)
	if len(inbounds) != 4 {
		t.Errorf("want 4 inbounds (socks, http, leap-internal-http, leap-internal-http-primary) in lab mode, got %d", len(inbounds))
	}
	for _, ib := range inbounds {
		m, _ := ib.(map[string]any)
		typ := m["type"].(string)
		if typ == "tun" || typ == "direct" {
			t.Errorf("lab mode should not produce tun/direct inbound, got %q", typ)
		}
	}

	// DNS / route / outbounds are still produced.
	if _, ok := doc["dns"]; !ok {
		t.Errorf("dns section missing in lab mode")
	}
	if _, ok := doc["route"]; !ok {
		t.Errorf("route section missing in lab mode")
	}
}

func TestRendererNoNodes(t *testing.T) {
	// Bootstrap path: no parsed outbounds yet. Should still write a valid
	// config (selector "out" → direct fallback).
	r, _ := newTestRenderer(t, true)
	data, err := r.Write(nil)
	if err != nil {
		t.Fatalf("Write empty: %v", err)
	}
	var doc map[string]any
	_ = json.Unmarshal(data, &doc)

	allOuts, _ := doc["outbounds"].([]any)
	// out + direct + dns-out + block = 4 (no urltest, no node entries).
	if len(allOuts) != 4 {
		t.Errorf("want 4 outbounds in bootstrap mode, got %d", len(allOuts))
	}
	for _, ob := range allOuts {
		m, _ := ob.(map[string]any)
		if m["tag"] == "out" {
			outs, _ := m["outbounds"].([]any)
			if len(outs) != 1 || outs[0] != "direct" {
				t.Errorf("bootstrap out should fall back to direct, got %v", outs)
			}
		}
	}

	if _, err := os.Stat(r.Path()); err != nil {
		t.Errorf("config not written: %v", err)
	}
}

func TestRendererPrimaryBackupSplit(t *testing.T) {
	// With ≥2 enabled subscriptions, the renderer should split outbounds by
	// the "<sub_name>/" tag prefix into urltest-primary (first sub) and
	// urltest-backup (the rest), and the selector "out" should list both.
	r, _ := newTestRenderer(t, true)
	r = r.WithSubscriptions([]config.SubscriptionEntry{
		{Name: "yuyun", Enabled: true},
		{Name: "backup", Enabled: true},
		{Name: "off", Enabled: false}, // disabled subs are ignored
	})

	outs := []subscribe.Outbound{
		{"type": "vmess", "tag": "yuyun/SG01", "server": "a", "server_port": 443, "uuid": "x"},
		{"type": "vmess", "tag": "yuyun/US01", "server": "b", "server_port": 443, "uuid": "y"},
		{"type": "trojan", "tag": "backup/HK01", "server": "c", "server_port": 443, "password": "p"},
	}
	data, err := r.Write(outs)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	var doc map[string]any
	_ = json.Unmarshal(data, &doc)

	allOuts, _ := doc["outbounds"].([]any)
	pools := map[string][]any{}
	var selOut map[string]any
	for _, ob := range allOuts {
		m, _ := ob.(map[string]any)
		tag, _ := m["tag"].(string)
		switch tag {
		case "out":
			selOut = m
		case "urltest-primary", "urltest-backup":
			pl, _ := m["outbounds"].([]any)
			pools[tag] = pl
		}
	}
	if selOut == nil {
		t.Fatal("missing selector out")
	}
	selList, _ := selOut["outbounds"].([]any)
	wantSel := []string{"urltest-primary", "urltest-backup", "direct"}
	if len(selList) != len(wantSel) {
		t.Fatalf("selector out members %v, want %v", selList, wantSel)
	}
	for i, want := range wantSel {
		if selList[i] != want {
			t.Errorf("selector out[%d] = %v, want %v", i, selList[i], want)
		}
	}
	if selOut["default"] != "urltest-primary" {
		t.Errorf("selector default = %v, want urltest-primary", selOut["default"])
	}

	if len(pools["urltest-primary"]) != 2 {
		t.Errorf("urltest-primary pool = %v, want 2 yuyun nodes", pools["urltest-primary"])
	}
	if len(pools["urltest-backup"]) != 1 {
		t.Errorf("urltest-backup pool = %v, want 1 backup node", pools["urltest-backup"])
	}
}

func TestRendererSingleSubscription(t *testing.T) {
	// One enabled sub: still emit urltest-primary (consistent naming for the
	// watchdog), no urltest-backup.
	r, _ := newTestRenderer(t, true)
	r = r.WithSubscriptions([]config.SubscriptionEntry{{Name: "yuyun", Enabled: true}})

	data, err := r.Write([]subscribe.Outbound{
		{"type": "vmess", "tag": "yuyun/SG01", "server": "a", "server_port": 443, "uuid": "x"},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	var doc map[string]any
	_ = json.Unmarshal(data, &doc)

	allOuts, _ := doc["outbounds"].([]any)
	tags := map[string]bool{}
	var selOut map[string]any
	for _, ob := range allOuts {
		m, _ := ob.(map[string]any)
		tags[m["tag"].(string)] = true
		if m["tag"] == "out" {
			selOut = m
		}
	}
	if !tags["urltest-primary"] {
		t.Errorf("missing urltest-primary in single-sub case")
	}
	if tags["urltest-backup"] {
		t.Errorf("urltest-backup should not exist in single-sub case")
	}
	selList, _ := selOut["outbounds"].([]any)
	if len(selList) != 2 {
		t.Errorf("single-sub selector should have 2 members (urltest-primary, direct), got %v", selList)
	}
}

func TestRendererWhitelistMode(t *testing.T) {
	r, _ := newTestRenderer(t, true)
	r.cfg.Route.Mode = "whitelist"
	r.cfg.Route.Whitelist = config.WhitelistConfig{
		Geosites:     config.StringList{"geosite-google", "geosite-openai"},
		Geoips:       config.StringList{"geoip-telegram"},
		DomainSuffix: []string{"claude.ai", "anthropic.com"},
		IPCIDR:       []string{"149.154.0.0/16", "91.108.0.0/16"},
	}

	data, err := r.Write([]subscribe.Outbound{
		{"type": "vmess", "tag": "n1", "server": "a", "server_port": 443, "uuid": "x"},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	var doc map[string]any
	_ = json.Unmarshal(data, &doc)

	dns, _ := doc["dns"].(map[string]any)
	if dns["final"] != "local" {
		t.Errorf("dns.final = %v, want local in whitelist mode", dns["final"])
	}
	dnsRules, _ := dns["rules"].([]any)
	foundFakeIPGeosite, foundFakeIPSuffix, foundQueryTypeFakeIP := false, false, false
	for _, rule := range dnsRules {
		m, _ := rule.(map[string]any)
		if m["server"] != "fakeip" {
			continue
		}
		if rs, ok := m["rule_set"].([]any); ok && len(rs) == 2 {
			foundFakeIPGeosite = true
		}
		if sx, ok := m["domain_suffix"].([]any); ok && len(sx) == 2 {
			foundFakeIPSuffix = true
		}
		if _, ok := m["query_type"]; ok {
			foundQueryTypeFakeIP = true
		}
		// Geoip / ip_cidr must NOT appear in DNS rules — DNS only matches domains.
		if _, ok := m["ip_cidr"]; ok {
			t.Errorf("dns: ip_cidr leaked into DNS rules")
		}
	}
	if !foundFakeIPGeosite {
		t.Errorf("dns: missing fakeip rule for whitelist geosites")
	}
	if !foundFakeIPSuffix {
		t.Errorf("dns: missing fakeip rule for whitelist domain_suffix")
	}
	if foundQueryTypeFakeIP {
		t.Errorf("dns: whitelist mode must not emit catch-all A/AAAA→fakeip rule")
	}

	route, _ := doc["route"].(map[string]any)
	if route["final"] != "direct" {
		t.Errorf("route.final = %v, want direct in whitelist mode", route["final"])
	}
	ruleSet, _ := route["rule_set"].([]any)
	// 2 cn (geosite + geoip) + 2 wl geosites + 1 wl geoip = 5
	if len(ruleSet) != 5 {
		t.Errorf("want 5 rule_set entries (cn + 2 wl geosites + 1 wl geoip), got %d", len(ruleSet))
	}
	gotTags := make(map[string]bool)
	for _, rs := range ruleSet {
		m, _ := rs.(map[string]any)
		gotTags[m["tag"].(string)] = true
	}
	for _, want := range []string{"geosite-cn", "geoip-cn", "geosite-google", "geosite-openai", "geoip-telegram"} {
		if !gotTags[want] {
			t.Errorf("rule_set missing tag=%q", want)
		}
	}

	routeRules, _ := route["rules"].([]any)
	foundRouteGeositeOut, foundRouteSuffixOut, foundRouteGeoipOut, foundRouteIPCIDROut := false, false, false, false
	for _, rule := range routeRules {
		m, _ := rule.(map[string]any)
		if m["outbound"] != "out" {
			continue
		}
		if rs, ok := m["rule_set"].([]any); ok {
			switch len(rs) {
			case 2:
				foundRouteGeositeOut = true
			case 1:
				if rs[0] == "geoip-telegram" {
					foundRouteGeoipOut = true
				}
			}
		}
		if sx, ok := m["domain_suffix"].([]any); ok && len(sx) == 2 {
			foundRouteSuffixOut = true
		}
		if cidrs, ok := m["ip_cidr"].([]any); ok && len(cidrs) == 2 {
			foundRouteIPCIDROut = true
		}
	}
	if !foundRouteGeositeOut {
		t.Errorf("route: missing rule_set→out rule for whitelist geosites")
	}
	if !foundRouteSuffixOut {
		t.Errorf("route: missing domain_suffix→out rule for whitelist suffixes")
	}
	if !foundRouteGeoipOut {
		t.Errorf("route: missing rule_set→out rule for whitelist geoips")
	}
	if !foundRouteIPCIDROut {
		t.Errorf("route: missing ip_cidr→out rule for whitelist CIDRs")
	}
}
