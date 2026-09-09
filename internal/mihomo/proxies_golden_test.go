package mihomo

import (
	"reflect"
	"testing"

	"github.com/leap-gateway/leap-gateway/internal/subscribe"
)

// Golden fixtures for outboundToProxy.
//
// Why golden (whole-map compare) rather than asserting individual keys:
// outboundToProxy is a hand-written per-protocol switch where the switch
// decides WHICH FIELDS SURVIVE. Any field the switch does not name is
// dropped in silence. A test that asserts only the keys it thought to check
// cannot catch that class of bug — it is the same blind spot the renderer
// has. Comparing the entire output map means a dropped field, an added
// field, and a renamed field all fail, and an intentional change has to be
// re-approved by editing the golden below.
//
// This is the regression net for 2026-09-09: hysteria2's nested obfs block
// was dropped, which turned 4 working 沃迪加速 专线 nodes into nodes that
// fail their handshake with "An error occurred in the delay test" —
// indistinguishable from dead, so the scorer evicted them on schedule.
//
// One row per protocol per OPTIONAL-FEATURE COMBINATION, not one row per
// protocol: the bug was a missing fixture combination as much as missing
// code.
func TestOutboundToProxyGolden(t *testing.T) {
	cases := []struct {
		name string
		in   subscribe.Outbound
		want map[string]any
	}{
		{
			name: "hysteria2 bare",
			in: subscribe.Outbound{
				"type": "hysteria2", "tag": "hy2-bare",
				"server": "a.example", "server_port": 443,
				"password": "pw",
				"tls":      map[string]any{"enabled": true, "server_name": "a.example"},
			},
			want: map[string]any{
				"name": "hy2-bare", "server": "a.example", "port": 443,
				"udp": true, "type": "hysteria2", "password": "pw",
				"sni": "a.example",
			},
		},
		{
			// THE regression row. Without applyObfsToClash both obfs keys
			// vanish and mihomo cannot decrypt the QUIC initial packet.
			name: "hysteria2 + salamander obfs",
			in: subscribe.Outbound{
				"type": "hysteria2", "tag": "hy2-obfs",
				"server": "p20.example", "server_port": 44044,
				"password": "pw",
				"obfs":     map[string]any{"type": "salamander", "password": "obfspw"},
				"tls":      map[string]any{"enabled": true, "server_name": "p20.example", "insecure": true},
			},
			want: map[string]any{
				"name": "hy2-obfs", "server": "p20.example", "port": 44044,
				"udp": true, "type": "hysteria2", "password": "pw",
				"obfs": "salamander", "obfs-password": "obfspw",
				"sni": "p20.example", "skip-cert-verify": true,
			},
		},
		{
			name: "hysteria2 + obfs type only (no obfs password)",
			in: subscribe.Outbound{
				"type": "hysteria2", "tag": "hy2-obfs-nopw",
				"server": "b.example", "server_port": 443,
				"password": "pw",
				"obfs":     map[string]any{"type": "salamander"},
			},
			want: map[string]any{
				"name": "hy2-obfs-nopw", "server": "b.example", "port": 443,
				"udp": true, "type": "hysteria2", "password": "pw",
				"obfs": "salamander",
			},
		},
		{
			// Documents a KNOWN LATENT HOLE, not desired behavior: mihomo
			// supports hysteria2 port hopping via `ports` + `hop-interval`
			// and the renderer drops both. No production subscription
			// carries them today (audited 2026-09-09: 145 nodes, zero
			// `ports`). When one does, this golden is the failing row that
			// says so instead of 4 more nodes silently scoring dead.
			name: "hysteria2 + port hopping — LATENT: ports/hop-interval dropped",
			in: subscribe.Outbound{
				"type": "hysteria2", "tag": "hy2-hop",
				"server": "c.example", "server_port": 443,
				"password": "pw",
				"ports":    "20000-30000", "hop_interval": "30s",
			},
			want: map[string]any{
				"name": "hy2-hop", "server": "c.example", "port": 443,
				"udp": true, "type": "hysteria2", "password": "pw",
			},
		},
		{
			name: "shadowsocks bare",
			in: subscribe.Outbound{
				"type": "shadowsocks", "tag": "ss-1",
				"server": "d.example", "server_port": 8388,
				"method": "aes-256-gcm", "password": "pw",
			},
			want: map[string]any{
				"name": "ss-1", "server": "d.example", "port": 8388,
				"udp": true, "type": "ss", "cipher": "aes-256-gcm", "password": "pw",
			},
		},
		{
			// LATENT HOLE: plugin / plugin-opts dropped (obfs-simple,
			// v2ray-plugin). Zero occurrences in production today.
			name: "shadowsocks + plugin — LATENT: plugin dropped",
			in: subscribe.Outbound{
				"type": "shadowsocks", "tag": "ss-plugin",
				"server": "e.example", "server_port": 8388,
				"method": "aes-256-gcm", "password": "pw",
				"plugin": "obfs-local",
				"plugin_opts": map[string]any{
					"mode": "http", "host": "bing.com",
				},
			},
			want: map[string]any{
				"name": "ss-plugin", "server": "e.example", "port": 8388,
				"udp": true, "type": "ss", "cipher": "aes-256-gcm", "password": "pw",
			},
		},
		{
			name: "vless + reality + flow",
			in: subscribe.Outbound{
				"type": "vless", "tag": "vless-reality",
				"server": "f.example", "server_port": 443,
				"uuid": "uuid-1", "flow": "xtls-rprx-vision",
				"packet_encoding": "xudp",
				"tls": map[string]any{
					"enabled": true, "server_name": "f.example",
					"reality": map[string]any{
						"enabled": true, "public_key": "pk", "short_id": "sid",
					},
					"utls": map[string]any{"enabled": true, "fingerprint": "chrome"},
				},
			},
			want: map[string]any{
				"name": "vless-reality", "server": "f.example", "port": 443,
				"udp": true, "type": "vless", "uuid": "uuid-1",
				"flow": "xtls-rprx-vision", "packet-encoding": "xudp",
				"tls": true, "servername": "f.example",
				"reality-opts":       map[string]any{"public-key": "pk", "short-id": "sid"},
				"client-fingerprint": "chrome",
			},
		},
		{
			// LATENT HOLE: tuic congestion-controller / udp-relay-mode
			// dropped. Zero tuic nodes in production today.
			name: "tuic + congestion control — LATENT: congestion_control dropped",
			in: subscribe.Outbound{
				"type": "tuic", "tag": "tuic-1",
				"server": "g.example", "server_port": 443,
				"uuid": "uuid-2", "password": "pw",
				"congestion_control": "bbr",
				"tls":                map[string]any{"enabled": true, "server_name": "g.example"},
			},
			want: map[string]any{
				"name": "tuic-1", "server": "g.example", "port": 443,
				"udp": true, "type": "tuic", "uuid": "uuid-2", "password": "pw",
				"sni": "g.example",
			},
		},
		{
			name: "anytls + client-fingerprint",
			in: subscribe.Outbound{
				"type": "anytls", "tag": "anytls-1",
				"server": "h.example", "server_port": 8443,
				"password": "pw", "client-fingerprint": "safari",
				"tls": map[string]any{"enabled": true, "server_name": "h.example"},
			},
			want: map[string]any{
				"name": "anytls-1", "server": "h.example", "port": 8443,
				"udp": true, "type": "anytls", "password": "pw",
				"tls": true, "sni": "h.example", "client-fingerprint": "safari",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := outboundToProxy(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("outboundToProxy mismatch\ngot:  %#v\nwant: %#v", got, tc.want)
			}
		})
	}
}

// TestObfsPaired is the paired preservation assertion, one direction each:
// present in input ⇒ present in output with equal value; absent in input ⇒
// absent in output with no spurious default. Shape borrowed from
// Smart-Config-Kit's validate-js-overwrites.js:744, which pairs
// "receives a deterministic client-fingerprint" with "existing
// client-fingerprint is preserved".
//
// Kept separate from the golden table because it states the invariant
// directly: the golden proves today's output, this proves the RULE, and the
// rule is what a future protocol addition must not break.
func TestObfsPaired(t *testing.T) {
	base := func() subscribe.Outbound {
		return subscribe.Outbound{
			"type": "hysteria2", "tag": "n",
			"server": "s.example", "server_port": 443, "password": "pw",
		}
	}

	t.Run("present is preserved", func(t *testing.T) {
		in := base()
		in["obfs"] = map[string]any{"type": "salamander", "password": "op"}
		got := outboundToProxy(in)
		if got["obfs"] != "salamander" {
			t.Errorf("obfs = %v, want salamander", got["obfs"])
		}
		if got["obfs-password"] != "op" {
			t.Errorf("obfs-password = %v, want op", got["obfs-password"])
		}
	})

	t.Run("absent yields no spurious default", func(t *testing.T) {
		got := outboundToProxy(base())
		if v, has := got["obfs"]; has {
			t.Errorf("obfs must be absent, got %v", v)
		}
		if v, has := got["obfs-password"]; has {
			t.Errorf("obfs-password must be absent, got %v", v)
		}
	})

	t.Run("unexpected shape does not panic and emits nothing", func(t *testing.T) {
		in := base()
		in["obfs"] = "salamander" // flat string: not the internal shape
		got := outboundToProxy(in)
		if v, has := got["obfs"]; has {
			t.Errorf("obfs must be absent for unhandled shape, got %v", v)
		}
	})
}

// TestObfsRoundTrip closes the loop across the parse paths and the renderer.
// proxies.go's header comment says it is the inverse of clash.go's
// clashProxyToOutbound; this asserts that claim mechanically rather than by
// comment, for the field that was dropped at BOTH ends plus the URI path
// that dropped it a third time.
//
// Drives the real entry point (subscribe.ParseBytes) rather than test-only
// exported wrappers, so the assertion covers the production path including
// format auto-detection and tag prefixing.
func TestObfsRoundTrip(t *testing.T) {
	clashYAML := []byte(`proxies:
  - name: rt
    type: hysteria2
    server: s.example
    port: 443
    password: pw
    obfs: salamander
    obfs-password: op
    sni: s.example
`)
	res, err := subscribe.ParseBytes("sub", "clash", clashYAML)
	if err != nil {
		t.Fatalf("clash parse: %v", err)
	}
	if len(res.Outbounds) != 1 {
		t.Fatalf("clash parse produced %d outbounds, want 1", len(res.Outbounds))
	}
	// The nested shape must survive the parse itself.
	obfs, ok := res.Outbounds[0]["obfs"].(map[string]any)
	if !ok {
		t.Fatalf("clash parse dropped obfs or used wrong shape: %#v",
			res.Outbounds[0]["obfs"])
	}
	if obfs["type"] != "salamander" || obfs["password"] != "op" {
		t.Errorf("clash parse obfs = %#v, want salamander/op", obfs)
	}
	got := outboundToProxy(res.Outbounds[0])
	if got["obfs"] != "salamander" || got["obfs-password"] != "op" {
		t.Errorf("clash→outbound→clash lost obfs: obfs=%v obfs-password=%v",
			got["obfs"], got["obfs-password"])
	}

	uriList := []byte(
		"hysteria2://pw@s.example:443?obfs=salamander&obfs-password=op&sni=s.example#rt")
	uriRes, err := subscribe.ParseBytes("sub", "uri", uriList)
	if err != nil {
		t.Fatalf("uri parse: %v", err)
	}
	if len(uriRes.Outbounds) != 1 {
		t.Fatalf("uri parse produced %d outbounds, want 1", len(uriRes.Outbounds))
	}
	gotURI := outboundToProxy(uriRes.Outbounds[0])
	if gotURI["obfs"] != "salamander" || gotURI["obfs-password"] != "op" {
		t.Errorf("uri→outbound→clash lost obfs: obfs=%v obfs-password=%v",
			gotURI["obfs"], gotURI["obfs-password"])
	}
}
