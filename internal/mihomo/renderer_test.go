package mihomo

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
	"github.com/leap-gateway/leap-gateway/internal/subscribe"
	"gopkg.in/yaml.v3"
)

func newTestRenderer(t *testing.T) *Renderer {
	t.Helper()
	cfg := config.DataPlaneConfig{
		// Use t.TempDir so Write's atomic-rename path is writable in tests.
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
		LogLevel:   "info",
	}
	cfg.ApplyDefaults()
	r := NewRenderer(cfg).WithNode(config.NodeConfig{
		ClientSubnet:  "10.8.13.0/24",
		Tun0GatewayIP: "10.8.13.1",
		EgressIface:   "ens18",
	})
	r.cfg.TUN.Enabled = true
	return r
}

func sampleOutbounds() []subscribe.Outbound {
	return []subscribe.Outbound{
		// trojan, no transport, simple TLS
		{
			"type": "trojan", "tag": "ash/🇺🇸US-IEPL-01",
			"server": "t7m2q9x4r8kap3-y6.edg3.org", "server_port": 32512,
			"password": "secret",
			"tls": map[string]any{
				"enabled": true, "server_name": "cas.baidu.com",
				"insecure": true,
			},
		},
		// vless+reality+utls (the bug-trigger case)
		{
			"type": "vless", "tag": "ctc-02/US-C30-01",
			"server": "us01.example.xyz", "server_port": 10050,
			"uuid": "ca70116e-e0b5-4736-8ec8-2a02cbd03a84",
			"flow": "xtls-rprx-vision", "packet_encoding": "xudp",
			"tls": map[string]any{
				"enabled": true, "server_name": "d1--ov-gotcha07.bilivideo.com",
				"reality": map[string]any{
					"enabled":    true,
					"public_key": "aGEQwyDcq--GiXgn5j5jmj5yhbbpMKIrpBtJxLkGIng",
					"short_id":   "a1b2c3d4e5f67890",
				},
				"utls": map[string]any{"enabled": true, "fingerprint": "firefox"},
			},
		},
		// vmess + ws transport
		{
			"type": "vmess", "tag": "yuyun/SG-01",
			"server": "vm.example.com", "server_port": 443,
			"uuid": "11111111-2222-3333-4444-555555555555", "alter_id": 0,
			"security": "auto",
			"tls":      map[string]any{"enabled": true, "server_name": "vm.example.com"},
			"transport": map[string]any{
				"type": "ws", "path": "/ws",
				"headers": map[string]any{"Host": "vm.example.com"},
			},
		},
		// hysteria2 (outside US — should NOT match NodePattern)
		{
			"type": "hysteria2", "tag": "yuyun/JP-Tokyo-01",
			"server": "jp.example.com", "server_port": 443,
			"password": "pwd",
			"tls":      map[string]any{"enabled": true, "server_name": "jp.example.com"},
		},
		// ss (US match)
		{
			"type": "shadowsocks", "tag": "cyberguard/🇺🇸 US-01",
			"server": "us.cg.example.com", "server_port": 8388,
			"method":   "aes-256-gcm",
			"password": "ss-pwd",
		},
		// internal types — must be filtered out
		{"type": "selector", "tag": "out-fake"},
		{"type": "urltest", "tag": "auto-fake"},
		{"type": "direct", "tag": "direct"},
	}
}

func TestRenderer_OverseasMode(t *testing.T) {
	r := newTestRenderer(t)
	r.cfg.URLTest.NodePattern = `美国|🇺🇸|\bUS`

	body, err := r.Write(sampleOutbounds())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("output not valid yaml: %v\n%s", err, body)
	}

	// Top-level keys
	for _, k := range []string{"mode", "log-level", "external-controller",
		"mixed-port", "dns", "tun", "sniffer", "proxies", "proxy-groups",
		"rule-providers", "rules"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("missing top-level key %q", k)
		}
	}

	// proxies — should have 5 (3 internal types filtered)
	proxies, _ := doc["proxies"].([]any)
	if len(proxies) != 5 {
		t.Errorf("proxies count = %d, want 5", len(proxies))
	}

	// proxy-groups: out, us-pool, pin
	groups, _ := doc["proxy-groups"].([]any)
	if len(groups) != 3 {
		t.Fatalf("proxy-groups count = %d, want 3", len(groups))
	}
	gNames := []string{}
	var usp map[string]any
	for _, g := range groups {
		m, _ := g.(map[string]any)
		gNames = append(gNames, m["name"].(string))
		if m["name"] == "us-pool" {
			usp = m
		}
	}
	if usp == nil {
		t.Fatal("us-pool group missing")
	}
	if usp["type"] != "load-balance" {
		t.Errorf("us-pool type = %v, want load-balance", usp["type"])
	}
	if usp["strategy"] != "consistent-hashing" {
		t.Errorf("us-pool strategy = %v, want consistent-hashing", usp["strategy"])
	}
	pool, _ := usp["proxies"].([]any)
	// Pattern matches 3 of 5: ash IEPL-01 (US), ctc-02 US-C30-01,
	// cyberguard 🇺🇸 US-01. yuyun JP and yuyun SG-01 don't match.
	if len(pool) != 3 {
		t.Errorf("us-pool members = %d, want 3", len(pool))
	}

	// Rules — overseas mode ends with MATCH,out
	rules, _ := doc["rules"].([]any)
	last, _ := rules[len(rules)-1].(string)
	if last != "MATCH,out" {
		t.Errorf("last rule = %q, want MATCH,out", last)
	}
	hasGeositeCN := false
	for _, ru := range rules {
		if s, _ := ru.(string); strings.Contains(s, "RULE-SET,geosite-cn,DIRECT") {
			hasGeositeCN = true
		}
	}
	if !hasGeositeCN {
		t.Error("missing geosite-cn DIRECT rule")
	}

	// rule-providers — 2 (geosite-cn, geoip-cn)
	rp, _ := doc["rule-providers"].(map[string]any)
	if len(rp) != 2 {
		t.Errorf("rule-providers count = %d, want 2 (overseas mode)", len(rp))
	}
	if _, ok := rp["geosite-cn"]; !ok {
		t.Error("rule-providers missing geosite-cn")
	}

	// TUN
	tun, _ := doc["tun"].(map[string]any)
	if tun["enable"] != true {
		t.Errorf("tun.enable = %v, want true", tun["enable"])
	}
	if tun["auto-route"] != false {
		t.Error("tun.auto-route MUST be false (leap-nft manages routing)")
	}
	dev, _ := tun["device"].(string)
	if len(dev) > 15 {
		t.Errorf("tun.device %q too long (%d > 15 IFNAMSIZ-1)", dev, len(dev))
	}
}

func TestRenderer_WhitelistMode(t *testing.T) {
	r := newTestRenderer(t)
	r.cfg.URLTest.NodePattern = `美国|🇺🇸|\bUS`
	r.cfg.Route.Mode = "whitelist"
	r.cfg.Route.Whitelist = config.WhitelistConfig{
		Geosites:     config.StringList{"geosite-anthropic", "geosite-openai"},
		Geoips:       config.StringList{"geoip-telegram"},
		DomainSuffix: []string{"claude.ai", "cursor.com"},
		IPCIDR:       []string{"149.154.0.0/16"},
	}

	body, err := r.Write(sampleOutbounds())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	var doc map[string]any
	_ = yaml.Unmarshal(body, &doc)

	rules, _ := doc["rules"].([]any)
	last, _ := rules[len(rules)-1].(string)
	if last != "MATCH,DIRECT" {
		t.Errorf("whitelist final rule = %q, want MATCH,DIRECT", last)
	}
	want := map[string]bool{
		"RULE-SET,geosite-anthropic,out":           false,
		"RULE-SET,geosite-openai,out":              false,
		"RULE-SET,geoip-telegram,out,no-resolve":   false,
		"DOMAIN-SUFFIX,claude.ai,out":              false,
		"DOMAIN-SUFFIX,cursor.com,out":             false,
		"IP-CIDR,149.154.0.0/16,out,no-resolve":    false,
	}
	for _, ru := range rules {
		s, _ := ru.(string)
		if _, ok := want[s]; ok {
			want[s] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("missing whitelist rule %q", k)
		}
	}

	// rule-providers should now include WL tags
	rp, _ := doc["rule-providers"].(map[string]any)
	for _, tag := range []string{"geosite-cn", "geoip-cn",
		"geosite-anthropic", "geosite-openai", "geoip-telegram"} {
		if _, ok := rp[tag]; !ok {
			t.Errorf("rule-providers missing %q in whitelist mode", tag)
		}
	}
}

func TestRenderer_VlessRealityRoundtrip(t *testing.T) {
	// Critical: tls.utls.fingerprint must round-trip back to client-fingerprint
	// in the emitted Clash proxy entry. This is the production bug we fixed
	// on the parser side; the renderer must preserve it the other direction.
	r := newTestRenderer(t)
	r.cfg.URLTest.NodePattern = ""

	body, err := r.Write(sampleOutbounds())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	var doc map[string]any
	_ = yaml.Unmarshal(body, &doc)

	proxies, _ := doc["proxies"].([]any)
	var vless map[string]any
	for _, p := range proxies {
		m, _ := p.(map[string]any)
		if m["type"] == "vless" {
			vless = m
			break
		}
	}
	if vless == nil {
		t.Fatal("no vless proxy in output")
	}
	if vless["client-fingerprint"] != "firefox" {
		t.Errorf("client-fingerprint = %v, want firefox", vless["client-fingerprint"])
	}
	if _, ok := vless["reality-opts"].(map[string]any); !ok {
		t.Error("vless missing reality-opts")
	}
	if vless["servername"] != "d1--ov-gotcha07.bilivideo.com" {
		t.Errorf("vless servername = %v", vless["servername"])
	}
}

func TestRenderer_EmptyPoolFallsBackToDirect(t *testing.T) {
	r := newTestRenderer(t)
	r.cfg.URLTest.NodePattern = "ZZZZ-NEVER-MATCHES"

	// With NO outbounds — bootstrap path. Pool is empty.
	body, err := r.Write(nil)
	if err != nil {
		t.Fatalf("Write empty: %v", err)
	}
	var doc map[string]any
	_ = yaml.Unmarshal(body, &doc)

	groups, _ := doc["proxy-groups"].([]any)
	// Should collapse to single 'out' selector → DIRECT
	if len(groups) != 1 {
		t.Errorf("empty bootstrap want 1 group, got %d", len(groups))
	}
	g, _ := groups[0].(map[string]any)
	if g["name"] != "out" {
		t.Errorf("group[0].name = %v, want out", g["name"])
	}
	pl, _ := g["proxies"].([]any)
	if len(pl) != 1 || pl[0] != "DIRECT" {
		t.Errorf("bootstrap group proxies = %v, want [DIRECT]", pl)
	}
}

// TestRenderer_FakeIPSkipSuffixes guards the production fix from 2026-06-05:
// internal services like ai.paigod.work resolve to CN/LAN IPs but aren't on
// geosite-cn. Mihomo's fakeip mode hands them 198.18.x.x by default, breaking
// DIRECT dial. Operators extend dns.fake_ip_skip_suffixes to carve them out;
// renderer normalizes each to "+.<suffix>" and adds to fake-ip-filter.
func TestRenderer_FakeIPSkipSuffixes(t *testing.T) {
	r := newTestRenderer(t)
	r.cfg.DNS.FakeIPSkipSuffixes = []string{
		"paigod.work",   // bare suffix — should normalize to +.paigod.work
		".feilian.cn",   // leading dot — should normalize to +.feilian.cn
		"+.intra.local", // already prefixed — pass through
	}
	body, err := r.Write(sampleOutbounds())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	var doc map[string]any
	_ = yaml.Unmarshal(body, &doc)
	dns, _ := doc["dns"].(map[string]any)
	filter, _ := dns["fake-ip-filter"].([]any)
	want := map[string]bool{
		"+.paigod.work": false,
		"+.feilian.cn":  false,
		"+.intra.local": false,
	}
	for _, e := range filter {
		s, _ := e.(string)
		if _, ok := want[s]; ok {
			want[s] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("fake-ip-filter missing %q", k)
		}
	}
}

// TestRenderer_SnifferEnabled guards the production fix from 2026-06-06:
// without sniffer, Chrome's DoH-acquired real IPs reach mihomo's TUN as raw
// IPs, geosite rules don't match, and load-balance hashes by dst IP — so a
// single Google session spreads across multiple proxy nodes and trips
// Google's session-anomaly response (the "Use secure DNS" diagnostic).
// sing-box has had the equivalent (sniff + sniff_override_destination) since
// day one. This test pins the sniffer block on so a future cleanup pass
// can't regress it silently.
func TestRenderer_SnifferEnabled(t *testing.T) {
	r := newTestRenderer(t)
	body, err := r.Write(sampleOutbounds())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	var doc map[string]any
	_ = yaml.Unmarshal(body, &doc)

	sn, ok := doc["sniffer"].(map[string]any)
	if !ok {
		t.Fatal("missing sniffer block")
	}
	if sn["enable"] != true {
		t.Errorf("sniffer.enable = %v, want true", sn["enable"])
	}
	if sn["override-destination"] != true {
		t.Error("sniffer.override-destination MUST be true (else dst stays raw IP, geosite never matches)")
	}
	if sn["parse-pure-ip"] != true {
		t.Error("sniffer.parse-pure-ip MUST be true (the DoH-bypass case is exactly pure IP)")
	}

	sniff, _ := sn["sniff"].(map[string]any)
	for _, proto := range []string{"TLS", "HTTP", "QUIC"} {
		if _, ok := sniff[proto]; !ok {
			t.Errorf("sniffer.sniff missing %s entry", proto)
		}
	}
}

// TestRenderer_DNSNameserverPolicy guards the 2026-06-06 DNS refactor:
// dropped fallback / fallback-filter (parallel-fan-out resolution) in favor
// of nameserver-policy (domain-keyed direct routing). Mirror of sing-box's
// `{rule_set: [geosite-cn], server: local}; final: remote`.
//
// Invariants:
//   - default nameserver = proxyDoH (not cnDoH) — overseas resolves through proxy
//   - nameserver-policy["rule-set:geosite-cn"] = cnDoH — CN domains short-circuit
//   - each fake-ip-skip suffix appears as "+.<suf>" in policy → cnDoH so
//     intranet domains (paigod.work etc.) resolve via the right side
//   - no `fallback` / `fallback-filter` keys (otherwise we're paying for
//     parallel queries again)
//   - proxy-server-nameserver still cnDoH (airport-node hostnames must
//     resolve direct, not through the proxy we're trying to set up)
func TestRenderer_DNSNameserverPolicy(t *testing.T) {
	r := newTestRenderer(t)
	r.cfg.DNS.CNDoH = []string{"https://doh.pub/dns-query"}
	r.cfg.DNS.ProxyDoH = []string{"tls://1.1.1.1:853"}
	r.cfg.DNS.FakeIPSkipSuffixes = []string{"paigod.work", ".feilian.cn"}

	body, err := r.Write(sampleOutbounds())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	var doc map[string]any
	_ = yaml.Unmarshal(body, &doc)
	dns, _ := doc["dns"].(map[string]any)

	// fallback gone
	if _, has := dns["fallback"]; has {
		t.Error("dns.fallback must be removed (defeats nameserver-policy savings)")
	}
	if _, has := dns["fallback-filter"]; has {
		t.Error("dns.fallback-filter must be removed")
	}

	// default nameserver = proxyDoH
	ns, _ := dns["nameserver"].([]any)
	if len(ns) != 1 || ns[0] != "tls://1.1.1.1:853" {
		t.Errorf("dns.nameserver = %v, want [tls://1.1.1.1:853] (proxyDoH default)", ns)
	}

	// proxy-server-nameserver = cnDoH (unchanged; airport-node resolution)
	psn, _ := dns["proxy-server-nameserver"].([]any)
	if len(psn) != 1 || psn[0] != "https://doh.pub/dns-query" {
		t.Errorf("proxy-server-nameserver = %v, want cnDoH list", psn)
	}

	// nameserver-policy keys
	pol, _ := dns["nameserver-policy"].(map[string]any)
	if pol == nil {
		t.Fatal("dns.nameserver-policy missing")
	}
	wantKeys := []string{
		"rule-set:geosite-cn",
		"+.paigod.work",
		"+.feilian.cn",
	}
	for _, k := range wantKeys {
		v, ok := pol[k]
		if !ok {
			t.Errorf("nameserver-policy missing key %q", k)
			continue
		}
		list, _ := v.([]any)
		if len(list) != 1 || list[0] != "https://doh.pub/dns-query" {
			t.Errorf("nameserver-policy[%q] = %v, want cnDoH list", k, v)
		}
	}
}

// Sanity: rendered yaml must round-trip through yaml unmarshal without error.
func TestRenderer_YAMLValid(t *testing.T) {
	r := newTestRenderer(t)
	r.cfg.URLTest.Interval = 90 * time.Second
	body, err := r.Write(sampleOutbounds())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("invalid yaml: %v\n---\n%s", err, body)
	}
	if !strings.Contains(string(body), "load-balance") {
		t.Error("expected 'load-balance' in rendered output")
	}
}

// TestRenderer_NamedPool covers the openai-pool path (1.1): a pool that
// gates on probes should produce (a) a load-balance group named after the
// pool, (b) the probe-out selector + leap-probe listener, (c) rules
// routing the pool's rule_sets to the pool instead of `out`.
func TestRenderer_NamedPool(t *testing.T) {
	r := newTestRenderer(t)
	r.cfg.URLTest.NodePattern = `美国|🇺🇸|\bUS`
	r.cfg.Route.Mode = "whitelist"
	r.cfg.Route.Whitelist = config.WhitelistConfig{
		Geosites: config.StringList{"geosite-openai", "geosite-github"},
	}
	r = r.WithPools([]config.PoolConfig{{
		Name:            "openai-pool",
		RequiresPassing: []string{"chatgpt.com"},
		RuleSets:        []string{"geosite-openai"},
	}})

	body, err := r.Write(sampleOutbounds())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("invalid yaml: %v\n%s", err, body)
	}

	// (a) openai-pool group exists, type load-balance.
	groups, _ := doc["proxy-groups"].([]any)
	var openai, probeOut map[string]any
	for _, g := range groups {
		m, _ := g.(map[string]any)
		switch m["name"] {
		case "openai-pool":
			openai = m
		case "probe-out":
			probeOut = m
		}
	}
	if openai == nil {
		t.Fatal("openai-pool group missing")
	}
	if openai["type"] != "load-balance" {
		t.Errorf("openai-pool type = %v, want load-balance", openai["type"])
	}
	// (b) probe-out selector + leap-probe listener.
	if probeOut == nil {
		t.Error("probe-out selector group missing (pool gates on probes)")
	}
	listeners, _ := doc["listeners"].([]any)
	if len(listeners) == 0 {
		t.Fatal("listeners block missing (expected leap-probe)")
	}
	l0, _ := listeners[0].(map[string]any)
	if l0["name"] != "leap-probe" || l0["proxy"] != "probe-out" {
		t.Errorf("listener[0] = %v, want leap-probe pinned to probe-out", l0)
	}

	// (c) rules: geosite-openai → openai-pool; geosite-github → out.
	rules, _ := doc["rules"].([]any)
	wantOpenAI, wantGithub := false, false
	for _, ru := range rules {
		s, _ := ru.(string)
		if s == "RULE-SET,geosite-openai,openai-pool" {
			wantOpenAI = true
		}
		if s == "RULE-SET,geosite-github,out" {
			wantGithub = true
		}
	}
	if !wantOpenAI {
		t.Error("missing rule RULE-SET,geosite-openai,openai-pool")
	}
	if !wantGithub {
		t.Error("missing rule RULE-SET,geosite-github,out (non-pooled tag should still route to out)")
	}
}

// TestRenderer_PoolMembersOverride checks RenderWithPools wires explicit
// member lists into the named pool group.
func TestRenderer_PoolMembersOverride(t *testing.T) {
	r := newTestRenderer(t)
	r.cfg.URLTest.NodePattern = ""
	r = r.WithPools([]config.PoolConfig{{
		Name:            "openai-pool",
		RequiresPassing: []string{"chatgpt.com"},
		RuleSets:        []string{"geosite-openai"},
	}})

	// Pick a real tag from sampleOutbounds.
	members := map[string][]string{"openai-pool": {"ash/🇺🇸US-IEPL-01"}}
	body, err := r.RenderWithPools(sampleOutbounds(), nil, members)
	if err != nil {
		t.Fatalf("RenderWithPools: %v", err)
	}
	var doc map[string]any
	_ = yaml.Unmarshal(body, &doc)
	groups, _ := doc["proxy-groups"].([]any)
	for _, g := range groups {
		m, _ := g.(map[string]any)
		if m["name"] == "openai-pool" {
			pl, _ := m["proxies"].([]any)
			if len(pl) != 1 || pl[0] != "ash/🇺🇸US-IEPL-01" {
				t.Errorf("openai-pool members = %v, want [ash/🇺🇸US-IEPL-01]", pl)
			}
			return
		}
	}
	t.Fatal("openai-pool group not found")
}
