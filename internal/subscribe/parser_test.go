package subscribe

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseClash(t *testing.T) {
	yaml := `
proxies:
  - name: "us-1"
    type: ss
    server: 1.2.3.4
    port: 8388
    cipher: aes-256-gcm
    password: "secret"
  - name: "tj-1"
    type: trojan
    server: example.com
    port: 443
    password: "tj-pwd"
    sni: example.com
    skip-cert-verify: true
  - name: "vm-1"
    type: vmess
    server: vm.example.com
    port: 443
    uuid: 11111111-2222-3333-4444-555555555555
    alterId: 0
    cipher: auto
    network: ws
    tls: true
    ws-opts:
      path: /ws
      headers:
        Host: vm.example.com
`
	out, err := parseClash([]byte(yaml))
	if err != nil {
		t.Fatalf("parseClash: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("want 3 outbounds, got %d", len(out))
	}
	if out[0]["type"] != "shadowsocks" || out[0]["server"] != "1.2.3.4" {
		t.Errorf("ss outbound malformed: %#v", out[0])
	}
	if out[1]["type"] != "trojan" {
		t.Errorf("trojan outbound malformed: %#v", out[1])
	}
	tls, ok := out[1]["tls"].(map[string]any)
	if !ok || tls["server_name"] != "example.com" || tls["insecure"] != true {
		t.Errorf("trojan tls malformed: %#v", out[1]["tls"])
	}
	if out[2]["type"] != "vmess" {
		t.Errorf("vmess outbound malformed: %#v", out[2])
	}
	tr, ok := out[2]["transport"].(map[string]any)
	if !ok || tr["type"] != "ws" || tr["path"] != "/ws" {
		t.Errorf("vmess transport malformed: %#v", out[2]["transport"])
	}
}

func TestParseClash_RealityRequiresUTLS(t *testing.T) {
	yaml := `
proxies:
  - name: "HK-Direct"
    type: vless
    server: 172.81.111.224
    port: 10009
    uuid: 2e2aa39e-dd37-496e-85e4-fc9689892743
    flow: xtls-rprx-vision
    tls: true
    skip-cert-verify: false
    client-fingerprint: chrome
    servername: www.ebay.com
    reality-opts:
      public-key: P5WXROxKQWdHF07lxmUXspUuCkoi-l6tR_2iG8NAD2c
      short-id: ca62d748
  - name: "TW-NoFP"
    type: vless
    server: tw.example.xyz
    port: 10009
    uuid: 2e2aa39e-dd37-496e-85e4-fc9689892743
    flow: xtls-rprx-vision
    tls: true
    servername: www.ebay.com
    reality-opts:
      public-key: qVSlZCRRCrsSmsVxFrKuPCQZTMdpVszD4nmjKDBnz2c
      short-id: ae59afcc
`
	out, err := parseClash([]byte(yaml))
	if err != nil {
		t.Fatalf("parseClash: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 outbounds, got %d", len(out))
	}
	for i, o := range out {
		tls := o["tls"].(map[string]any)
		if _, ok := tls["reality"].(map[string]any); !ok {
			t.Errorf("outbound[%d] missing tls.reality", i)
		}
		utls, ok := tls["utls"].(map[string]any)
		if !ok {
			t.Fatalf("outbound[%d] missing tls.utls — sing-box would FATAL", i)
		}
		if utls["enabled"] != true {
			t.Errorf("outbound[%d] tls.utls.enabled = %v", i, utls["enabled"])
		}
		if fp, _ := utls["fingerprint"].(string); fp == "" {
			t.Errorf("outbound[%d] missing tls.utls.fingerprint", i)
		}
	}
	if fp := out[0]["tls"].(map[string]any)["utls"].(map[string]any)["fingerprint"]; fp != "chrome" {
		t.Errorf("explicit fingerprint dropped: %v", fp)
	}
}

func TestParseURIList_Base64(t *testing.T) {
	lines := []string{
		"vmess://" + base64.StdEncoding.EncodeToString([]byte(
			`{"v":"2","ps":"vm-uri","add":"1.2.3.4","port":"443","id":"u","aid":"0","net":"ws","host":"h","path":"/p","tls":"tls"}`)),
		"trojan://pwd@example.com:443?sni=example.com#tj-uri",
		"ss://" + base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:pw@1.2.3.4:8388")) + "#ss-uri",
	}
	body := base64.StdEncoding.EncodeToString([]byte(strings.Join(lines, "\n")))
	out, err := parseURIList([]byte(body))
	if err != nil {
		t.Fatalf("parseURIList: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("want 3 outbounds, got %d: %#v", len(out), out)
	}
	if out[0]["type"] != "vmess" || out[0]["server"] != "1.2.3.4" {
		t.Errorf("vmess uri malformed: %#v", out[0])
	}
	if out[1]["type"] != "trojan" || out[1]["password"] != "pwd" {
		t.Errorf("trojan uri malformed: %#v", out[1])
	}
	if out[2]["type"] != "shadowsocks" || out[2]["method"] != "aes-256-gcm" {
		t.Errorf("ss uri malformed: %#v", out[2])
	}
}

// TestParseVlessRealityURI verifies that every query parameter a VLESS+REALITY
// URI can carry lands in the expected Outbound field. This is the regression
// guard against silent field drops across uri.go / clash.go / proxies.go.
func TestParseVlessRealityURI(t *testing.T) {
	uri := "vless://2e2aa39e-dd37-496e-85e4-fc9689892743@example.com:31513" +
		"?type=tcp&security=reality&flow=xtls-rprx-vision" +
		"&fp=chrome&sni=v5-dy-e.ixigua.com" +
		"&pbk=kwsYRITkG2Z9WAhuTAPoP3eqG-mpFkfnXoQV4cjsRx0&sid=07c418eb" +
		"&alpn=h2%2Chttp%2F1.1&insecure=0" +
		"#%F0%9F%87%BA%F0%9F%87%B8US-01"

	out, err := parseURIList([]byte(uri))
	if err != nil {
		t.Fatalf("parseURIList: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 outbound, got %d", len(out))
	}
	o := out[0]

	check := func(field string, want any) {
		t.Helper()
		if o[field] != want {
			t.Errorf("field %q: want %v, got %v", field, want, o[field])
		}
	}

	check("type", "vless")
	check("server", "example.com")
	check("server_port", 31513)
	check("uuid", "2e2aa39e-dd37-496e-85e4-fc9689892743")
	check("flow", "xtls-rprx-vision")
	// client-fingerprint is a sibling key (not inside tls), matching how
	// clash.go and proxies.go handle it for mihomo rendering.
	check("client-fingerprint", "chrome")

	tls, ok := o["tls"].(map[string]any)
	if !ok {
		t.Fatalf("tls block missing or wrong type: %#v", o["tls"])
	}
	if tls["server_name"] != "v5-dy-e.ixigua.com" {
		t.Errorf("tls.server_name: want v5-dy-e.ixigua.com, got %v", tls["server_name"])
	}
	reality, ok := tls["reality"].(map[string]any)
	if !ok {
		t.Fatalf("tls.reality block missing: %#v", tls)
	}
	if reality["public_key"] != "kwsYRITkG2Z9WAhuTAPoP3eqG-mpFkfnXoQV4cjsRx0" {
		t.Errorf("reality.public_key wrong: %v", reality["public_key"])
	}
	if reality["short_id"] != "07c418eb" {
		t.Errorf("reality.short_id wrong: %v", reality["short_id"])
	}
}

func TestParseSIP008(t *testing.T) {
	body := []byte(`[{"remarks":"a","server":"1.2.3.4","server_port":8388,"method":"aes-256-gcm","password":"p"}]`)
	out, err := parseSIP008(body)
	if err != nil {
		t.Fatalf("parseSIP008: %v", err)
	}
	if len(out) != 1 || out[0]["type"] != "shadowsocks" || out[0]["password"] != "p" {
		t.Errorf("sip008 malformed: %#v", out)
	}
}

func TestParseSingBox(t *testing.T) {
	body := []byte(`{"outbounds":[
		{"type":"vmess","tag":"a","server":"1.1.1.1","server_port":443,"uuid":"x"},
		{"type":"direct","tag":"d"},
		{"type":"selector","tag":"s","outbounds":["a"]}
	]}`)
	out, err := parseSingBox(body)
	if err != nil {
		t.Fatalf("parseSingBox: %v", err)
	}
	if len(out) != 1 || out[0]["tag"] != "a" {
		t.Errorf("singbox passthrough should keep only the vmess node, got %#v", out)
	}
}

// parseSingBox is the only parser that passes outbounds through instead of
// rebuilding them, so it was the only one accepting an entry with no
// server / server_port. Those reached the renderer as `server: <nil>`, and
// — more damaging — an empty server is the bootstrap key for fault-domain
// grouping, where it silently buckets unrelated nodes into one domain.
// Assert the entries are dropped, and that dropping one does not discard
// the valid siblings alongside it.
func TestParseSingBox_RejectsMissingServerOrPort(t *testing.T) {
	body := []byte(`{"outbounds":[
		{"type":"vmess","tag":"ok","server":"1.1.1.1","server_port":443,"uuid":"x"},
		{"type":"vmess","tag":"no-server","server_port":443,"uuid":"x"},
		{"type":"vmess","tag":"empty-server","server":"","server_port":443,"uuid":"x"},
		{"type":"vmess","tag":"no-port","server":"2.2.2.2","uuid":"x"},
		{"type":"vmess","tag":"zero-port","server":"3.3.3.3","server_port":0,"uuid":"x"},
		{"type":"trojan","tag":"ok2","server":"4.4.4.4","server_port":8443,"password":"p"}
	]}`)
	out, err := parseSingBox(body)
	if err != nil {
		t.Fatalf("parseSingBox: %v", err)
	}
	got := map[string]bool{}
	for _, o := range out {
		tag, _ := o["tag"].(string)
		got[tag] = true
	}
	for _, want := range []string{"ok", "ok2"} {
		if !got[want] {
			t.Errorf("valid outbound %q was dropped; got %v", want, got)
		}
	}
	for _, bad := range []string{"no-server", "empty-server", "no-port", "zero-port"} {
		if got[bad] {
			t.Errorf("outbound %q has no usable server/port and must be dropped; got %v", bad, got)
		}
	}
	if len(out) != 2 {
		t.Errorf("expected exactly the 2 valid outbounds, got %d (%v)", len(out), got)
	}
}

// server_port arriving as a JSON string is normal in the wild (some
// providers quote it). getInt handles that, so such an entry must survive
// — the validation is "no usable port", not "port is not a JSON number".
func TestParseSingBox_AcceptsStringPort(t *testing.T) {
	body := []byte(`{"outbounds":[
		{"type":"vmess","tag":"quoted","server":"1.1.1.1","server_port":"443","uuid":"x"}
	]}`)
	out, err := parseSingBox(body)
	if err != nil {
		t.Fatalf("parseSingBox: %v", err)
	}
	if len(out) != 1 || out[0]["tag"] != "quoted" {
		t.Errorf("string server_port must be accepted, got %#v", out)
	}
}

func TestDetectAndParse_Clash(t *testing.T) {
	body := []byte("proxies:\n  - name: x\n    type: ss\n    server: 1.1.1.1\n    port: 8388\n    cipher: aes-256-gcm\n    password: p\n")
	out, format, err := detectAndParse(body)
	if err != nil {
		t.Fatal(err)
	}
	if format != "clash" || len(out) != 1 {
		t.Errorf("want clash 1 node, got format=%s len=%d", format, len(out))
	}
}

func TestFilterOutbounds(t *testing.T) {
	in := []Outbound{
		{"type": "vless", "tag": "hk-01"},
		{"type": "anytls", "tag": "hk-02-anytls"},
		{"type": "hysteria2", "tag": "剩余流量：872.84 GB"},
		{"type": "hysteria2", "tag": "套餐到期：长期有效"},
		{"type": "hysteria2", "tag": "jp-01"},
		{"type": "vless", "tag": "官网 https://example.com"},
		{"type": "hysteria2", "tag": "节点超时请尝试重新导入"},
		{"type": "hysteria2", "tag": "收藏导航https://cloudfisher.icu"},
		{"type": "vless", "tag": "🇭🇰 香港01 境外中转"},
	}
	got := filterOutbounds(in)
	if len(got) != 4 {
		t.Fatalf("want 4 kept, got %d: %#v", len(got), got)
	}
	wantTags := []string{"hk-01", "hk-02-anytls", "jp-01", "🇭🇰 香港01 境外中转"}
	for i, w := range wantTags {
		if got[i].Tag() != w {
			t.Errorf("[%d] want %q, got %q", i, w, got[i].Tag())
		}
	}
}
