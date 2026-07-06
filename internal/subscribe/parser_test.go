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
