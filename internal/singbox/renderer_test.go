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

	// Inbounds: tun-in + dns-in (no socks/http because we didn't set them).
	inbounds, _ := doc["inbounds"].([]any)
	if len(inbounds) != 2 {
		t.Errorf("want 2 inbounds (tun, dns), got %d", len(inbounds))
	}
	tags := make(map[string]bool)
	for _, ib := range inbounds {
		m, _ := ib.(map[string]any)
		tags[m["tag"].(string)] = true
	}
	if !tags["tun-in"] || !tags["dns-in"] {
		t.Errorf("missing tun-in or dns-in: %v", tags)
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
	for _, want := range []string{"out", "urltest", "n1", "n2", "direct", "dns-out", "block"} {
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

	// Lab mode → no tun-in / dns-in, just socks + http.
	inbounds, _ := doc["inbounds"].([]any)
	if len(inbounds) != 2 {
		t.Errorf("want 2 inbounds (socks, http) in lab mode, got %d", len(inbounds))
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
