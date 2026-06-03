package subscribe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

// minimalClash is the smallest Clash-YAML body the parser accepts, with one
// trojan proxy. parseClash will produce 1 outbound from this.
const minimalClash = `proxies:
- {name: probe-trojan, type: trojan, server: example.com, port: 443, password: x, sni: example.com}
`

// emptyClash mimics yuyun's anti-Clash-UA gimmick: status 200, valid YAML,
// proxies list empty.
const emptyClash = `proxies: []
`

// minimalSingBox is the smallest sing-box JSON the parser accepts.
const minimalSingBox = `{"outbounds":[{"type":"trojan","tag":"probe-trojan","server":"example.com","server_port":443,"password":"x"}]}`

// dualBehavior wraps two http.HandlerFuncs keyed by the User-Agent prefix.
// Returns the matching one or 500 if neither matches.
func dualBehavior(t *testing.T, clashHandler, singboxHandler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		switch {
		case strings.Contains(strings.ToLower(ua), "clash"):
			if clashHandler != nil {
				clashHandler(w, r)
				return
			}
		case strings.Contains(strings.ToLower(ua), "sing-box"):
			if singboxHandler != nil {
				singboxHandler(w, r)
				return
			}
		}
		http.Error(w, "no handler for ua "+ua, http.StatusInternalServerError)
	}))
}

// TestRefresh_NoFallbackOnHappyPath: Clash UA already works → no fallback.
func TestRefresh_NoFallbackOnHappyPath(t *testing.T) {
	t.Parallel()
	var clashHits, singboxHits atomic.Int32
	srv := dualBehavior(t,
		func(w http.ResponseWriter, _ *http.Request) {
			clashHits.Add(1)
			_, _ = w.Write([]byte(minimalClash))
		},
		func(w http.ResponseWriter, _ *http.Request) {
			singboxHits.Add(1)
			_, _ = w.Write([]byte(minimalSingBox))
		},
	)
	defer srv.Close()

	mgr := NewManagerWithFetch([]config.SubscriptionEntry{
		{Name: "ok", URL: srv.URL, Format: "auto", Enabled: true},
	}, 5*time.Second, "ClashforWindows/0.20.39")

	results, err := mgr.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(results) != 1 || len(results[0].Outbounds) != 1 {
		t.Fatalf("results = %v", results)
	}
	if clashHits.Load() != 1 || singboxHits.Load() != 0 {
		t.Errorf("expected 1 Clash + 0 sing-box hits, got Clash=%d sing-box=%d",
			clashHits.Load(), singboxHits.Load())
	}
}

// TestRefresh_FallbackOnZeroNodes: yuyun-class — Clash UA returns 200 with
// empty proxies; sing-box UA returns full content. Refresh should retry,
// pick sing-box, and fire the discovery callback with sing-box/1.10.7.
func TestRefresh_FallbackOnZeroNodes(t *testing.T) {
	t.Parallel()
	srv := dualBehavior(t,
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(emptyClash))
		},
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(minimalSingBox))
		},
	)
	defer srv.Close()

	var cbMu sync.Mutex
	var cbName, cbUA string
	mgr := NewManagerWithFetch([]config.SubscriptionEntry{
		{Name: "yuyun-like", URL: srv.URL, Format: "auto", Enabled: true},
	}, 5*time.Second, "ClashforWindows/0.20.39").
		WithUADiscoveryCallback(func(name, ua string) {
			cbMu.Lock()
			cbName, cbUA = name, ua
			cbMu.Unlock()
		})

	results, err := mgr.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(results) != 1 || len(results[0].Outbounds) != 1 {
		t.Fatalf("results = %+v", results)
	}
	cbMu.Lock()
	defer cbMu.Unlock()
	if cbName != "yuyun-like" || cbUA != "sing-box/1.10.7" {
		t.Errorf("callback fired with name=%q ua=%q, want yuyun-like / sing-box/1.10.7",
			cbName, cbUA)
	}
}

// TestRefresh_FallbackOn5xx: ash-class — Clash UA returns 500; sing-box UA
// returns full content. Same fallback path as zero-nodes, different trigger.
func TestRefresh_FallbackOn5xx(t *testing.T) {
	t.Parallel()
	srv := dualBehavior(t,
		func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "wrong UA", http.StatusInternalServerError)
		},
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(minimalSingBox))
		},
	)
	defer srv.Close()

	var cbMu sync.Mutex
	var cbUA string
	mgr := NewManagerWithFetch([]config.SubscriptionEntry{
		{Name: "ash-like", URL: srv.URL, Format: "auto", Enabled: true},
	}, 5*time.Second, "ClashforWindows/0.20.39").
		WithUADiscoveryCallback(func(_, ua string) {
			cbMu.Lock()
			cbUA = ua
			cbMu.Unlock()
		})

	results, err := mgr.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	cbMu.Lock()
	defer cbMu.Unlock()
	if cbUA != "sing-box/1.10.7" {
		t.Errorf("callback fired with ua=%q, want sing-box/1.10.7", cbUA)
	}
}

// TestRefresh_NoFallbackOn4xx: 403/401/etc are auth/token problems —
// retrying with a different UA is futile and just doubles upstream load.
// Ensure no retry, no callback, error surfaces from primary.
func TestRefresh_NoFallbackOn4xx(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "token bad", http.StatusForbidden)
	}))
	defer srv.Close()

	cbFired := false
	mgr := NewManagerWithFetch([]config.SubscriptionEntry{
		{Name: "rec", URL: srv.URL, Format: "auto", Enabled: true},
	}, 5*time.Second, "ClashforWindows/0.20.39").
		WithUADiscoveryCallback(func(_, _ string) { cbFired = true })

	_, err := mgr.Refresh(context.Background())
	if err == nil {
		t.Fatal("expected fetch error on 403")
	}
	if hits.Load() != 1 {
		t.Errorf("expected exactly 1 upstream hit on 403, got %d", hits.Load())
	}
	if cbFired {
		t.Error("discovery callback must not fire on 4xx")
	}
}

// TestRefresh_NoFallbackWhenOperatorSetUA: operator override is sticky.
// Even a 0-nodes result with explicit UA shouldn't trigger fallback —
// the operator presumably knows what they're doing, and we don't want to
// silently overwrite their config decision.
func TestRefresh_NoFallbackWhenOperatorSetUA(t *testing.T) {
	t.Parallel()
	var clashHits, singboxHits atomic.Int32
	srv := dualBehavior(t,
		func(w http.ResponseWriter, _ *http.Request) {
			clashHits.Add(1)
			_, _ = w.Write([]byte(emptyClash))
		},
		func(w http.ResponseWriter, _ *http.Request) {
			singboxHits.Add(1)
			_, _ = w.Write([]byte(minimalSingBox))
		},
	)
	defer srv.Close()

	cbFired := false
	mgr := NewManagerWithFetch([]config.SubscriptionEntry{
		{Name: "explicit", URL: srv.URL, Format: "auto", Enabled: true,
			UserAgent: "ClashforWindows/0.20.39"},
	}, 5*time.Second, "sing-box/1.10.7").
		WithUADiscoveryCallback(func(_, _ string) { cbFired = true })

	_, _ = mgr.Refresh(context.Background())
	if clashHits.Load() != 1 || singboxHits.Load() != 0 {
		t.Errorf("expected 1 Clash + 0 sing-box (no fallback when explicit UA), got %d/%d",
			clashHits.Load(), singboxHits.Load())
	}
	if cbFired {
		t.Error("callback must not fire when entry has explicit UserAgent")
	}
}

func TestAlternateUA(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"", "sing-box/1.10.7"},
		{"ClashforWindows/0.20.39", "sing-box/1.10.7"},
		{"clash.meta/v1.18.0", "sing-box/1.10.7"},
		{"ClashX Pro/1.92.1", "sing-box/1.10.7"},
		{"Stash/2.6.0", "sing-box/1.10.7"},
		{"mihomo/Meta v1.18.0", "sing-box/1.10.7"},
		{"leap-gateway/0.1", "sing-box/1.10.7"},
		{"sing-box/1.10.7", "ClashforWindows/0.20.39"},
		{"sing-box/1.18", "ClashforWindows/0.20.39"},
		{"Mozilla/5.0", ""}, // unknown family: no fallback
	}
	for _, c := range cases {
		if got := alternateUA(c.in); got != c.want {
			t.Errorf("alternateUA(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
