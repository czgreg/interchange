package mihomo

import (
	"testing"

	"github.com/leap-gateway/leap-gateway/internal/subscribe"
)

func TestNormalizeWSHeaders(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want map[string]string
	}{
		{
			name: "string passthrough",
			in:   map[string]any{"Host": "foo.example"},
			want: map[string]string{"Host": "foo.example"},
		},
		{
			name: "list takes first element (cyberguard case)",
			in:   map[string]any{"Host": []any{"cghk1.example"}},
			want: map[string]string{"Host": "cghk1.example"},
		},
		{
			name: "list multiple values: first wins, others dropped",
			in:   map[string]any{"Host": []any{"a.example", "b.example"}},
			want: map[string]string{"Host": "a.example"},
		},
		{
			name: "[]string list",
			in:   map[string]any{"Host": []string{"x.example"}},
			want: map[string]string{"Host": "x.example"},
		},
		{
			name: "empty list dropped",
			in:   map[string]any{"Host": []any{}},
			want: map[string]string{},
		},
		{
			name: "nil dropped",
			in:   map[string]any{"Host": nil},
			want: map[string]string{},
		},
		{
			name: "empty string dropped",
			in:   map[string]any{"Host": ""},
			want: map[string]string{},
		},
		{
			name: "mixed keys",
			in: map[string]any{
				"Host":       []any{"h.example"},
				"User-Agent": "ua/1",
			},
			want: map[string]string{
				"Host":       "h.example",
				"User-Agent": "ua/1",
			},
		},
		{
			name: "non-string non-list scalar fmt.Sprint",
			in:   map[string]any{"X-Port": 443},
			want: map[string]string{"X-Port": "443"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeWSHeaders(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("size: got %d want %d (%#v vs %#v)", len(got), len(tc.want), got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("key %q: got %q want %q", k, got[k], v)
				}
			}
		})
	}
}

// TestOutboundToProxy_VlessRealityFingerprint verifies that the URI parse
// path's client-fingerprint sibling key (set by uri.go from fp=) survives
// the outboundToProxy render. This is the regression guard for the ash
// subscription incident: URI format carries fp=chrome, clash.go path carries
// it via tls.utls.fingerprint — both must land as client-fingerprint in the
// mihomo proxy entry.
func TestOutboundToProxy_VlessRealityFingerprint(t *testing.T) {
	// Simulate the URI parse path: fp= lands as a sibling key (no tls.utls).
	uriPath := subscribe.Outbound{
		"type":               "vless",
		"tag":                "ash-us-02",
		"server":             "example.com",
		"server_port":        31513,
		"uuid":               "2e2aa39e-dd37-496e-85e4-fc9689892743",
		"flow":               "xtls-rprx-vision",
		"client-fingerprint": "chrome",
		"tls": map[string]any{
			"enabled":     true,
			"server_name": "v5-dy-e.ixigua.com",
			"reality": map[string]any{
				"enabled":    true,
				"public_key": "kwsYRITkG2Z9WAhuTAPoP3eqG-mpFkfnXoQV4cjsRx0",
				"short_id":   "07c418eb",
			},
		},
	}
	p := outboundToProxy(uriPath)
	if p == nil {
		t.Fatal("outboundToProxy returned nil")
	}
	if p["client-fingerprint"] != "chrome" {
		t.Errorf("URI path: client-fingerprint = %v, want chrome", p["client-fingerprint"])
	}

	// Simulate the Clash YAML parse path: fingerprint in tls.utls.
	clashPath := subscribe.Outbound{
		"type":        "vless",
		"tag":         "ash-us-02",
		"server":      "example.com",
		"server_port": 31513,
		"uuid":        "2e2aa39e-dd37-496e-85e4-fc9689892743",
		"flow":        "xtls-rprx-vision",
		"tls": map[string]any{
			"enabled":     true,
			"server_name": "v5-dy-e.ixigua.com",
			"utls": map[string]any{
				"enabled":     true,
				"fingerprint": "chrome",
			},
			"reality": map[string]any{
				"enabled":    true,
				"public_key": "kwsYRITkG2Z9WAhuTAPoP3eqG-mpFkfnXoQV4cjsRx0",
				"short_id":   "07c418eb",
			},
		},
	}
	p2 := outboundToProxy(clashPath)
	if p2 == nil {
		t.Fatal("outboundToProxy returned nil (clash path)")
	}
	if p2["client-fingerprint"] != "chrome" {
		t.Errorf("Clash path: client-fingerprint = %v, want chrome", p2["client-fingerprint"])
	}
}

// End-to-end at the renderer boundary: a vless+ws outbound carrying the
// list-shape Host header (the cyberguard subscription shape that crashlooped
// 89 on 2026-06-09) must come out of outboundToProxy as a plain string under
// ws-opts.headers.Host. This locks the contract with mihomo, which refuses
// `headers[Host]` of type []any at startup.
func TestOutboundToProxy_WSHeadersHostList(t *testing.T) {
	o := subscribe.Outbound{
		"type":        "vless",
		"tag":         "cg-1",
		"server":      "example.com",
		"server_port": 443,
		"uuid":        "250e6b39-774b-4b19-8e7c-07a4c256a483",
		"tls": map[string]any{
			"enabled":     true,
			"server_name": "cghk1.example.com",
		},
		"transport": map[string]any{
			"type": "ws",
			"path": "/cghk1",
			"headers": map[string]any{
				"Host": []any{"cghk1.example.com"},
			},
		},
	}
	p := outboundToProxy(o)
	if p == nil {
		t.Fatal("outboundToProxy returned nil")
	}
	wsopts, ok := p["ws-opts"].(map[string]any)
	if !ok {
		t.Fatalf("ws-opts missing or wrong type: %#v", p["ws-opts"])
	}
	headers, ok := wsopts["headers"].(map[string]string)
	if !ok {
		t.Fatalf("ws-opts.headers must be map[string]string, got %T: %#v", wsopts["headers"], wsopts["headers"])
	}
	if headers["Host"] != "cghk1.example.com" {
		t.Errorf("Host: got %q want %q", headers["Host"], "cghk1.example.com")
	}
}
