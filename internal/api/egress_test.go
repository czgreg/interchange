package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProbe builds a probeFn that returns the given egress info on each
// call, respecting the supplied delay and a counter so tests can assert
// invocation counts.
func fakeProbe(t *testing.T, ip, country string, delay time.Duration, calls *atomic.Int32) func(context.Context, string, string) (*egressInfo, error) {
	t.Helper()
	return func(ctx context.Context, _, viaNode string) (*egressInfo, error) {
		calls.Add(1)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &egressInfo{
			IP:        ip,
			Country:   country,
			CheckedAt: time.Now(),
			ViaNode:   viaNode,
		}, nil
	}
}

func TestEgress_FirstCallNilThenPopulated(t *testing.T) {
	t.Parallel()
	c := newEgressCache()
	var calls atomic.Int32
	c.probeFn = fakeProbe(t, "1.2.3.4", "US", 10*time.Millisecond, &calls)

	if got := c.GetOrRefresh("urltest-primary", "http://x", "node-A"); got != nil {
		t.Fatalf("expected nil on first call, got %+v", got)
	}
	// Poll for the cache to populate. Probe delay is 10ms so 200ms is
	// generous slack across CI / loaded test machines.
	deadline := time.Now().Add(200 * time.Millisecond)
	var got *egressInfo
	for time.Now().Before(deadline) {
		got = c.GetOrRefresh("urltest-primary", "http://x", "node-A")
		if got != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got == nil || got.IP != "1.2.3.4" || got.Country != "US" {
		t.Fatalf("expected populated cache, got %+v", got)
	}
	if got.Stale {
		t.Errorf("Stale should be false when via_node matches")
	}
}

func TestEgress_TTLNoRefreshWithinWindow(t *testing.T) {
	t.Parallel()
	c := newEgressCache()
	var calls atomic.Int32
	c.probeFn = fakeProbe(t, "1.2.3.4", "US", 5*time.Millisecond, &calls)

	c.GetOrRefresh("p", "http://x", "node-A")
	for i := 0; i < 50 && calls.Load() < 1; i++ {
		time.Sleep(2 * time.Millisecond)
	}
	for i := 0; i < 5; i++ {
		c.GetOrRefresh("p", "http://x", "node-A")
	}
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (no re-probe within TTL)", calls.Load())
	}
}

func TestEgress_InflightDedupe(t *testing.T) {
	t.Parallel()
	c := newEgressCache()
	var calls atomic.Int32
	c.probeFn = fakeProbe(t, "9.9.9.9", "JP", 80*time.Millisecond, &calls)

	const N = 20
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.GetOrRefresh("p", "http://x", "node-A")
		}()
	}
	wg.Wait()
	time.Sleep(120 * time.Millisecond)
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want exactly 1 (inflight dedupe failed)", calls.Load())
	}
}

func TestEgress_StaleOnViaNodeMismatch(t *testing.T) {
	t.Parallel()
	c := newEgressCache()
	var calls atomic.Int32
	c.probeFn = fakeProbe(t, "5.6.7.8", "DE", 5*time.Millisecond, &calls)

	c.GetOrRefresh("p", "http://x", "node-A")
	for i := 0; i < 50 && calls.Load() < 1; i++ {
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)

	got := c.GetOrRefresh("p", "http://x", "node-B")
	if got == nil {
		t.Fatal("expected populated cache, got nil")
	}
	if !got.Stale {
		t.Errorf("expected Stale=true when current_node (node-B) != via_node (node-A), got %+v", got)
	}
}

func TestEgress_ErrorMarksStaleNotOverwrite(t *testing.T) {
	t.Parallel()
	c := newEgressCache()
	var ok atomic.Bool
	ok.Store(true)
	c.probeFn = func(ctx context.Context, _, viaNode string) (*egressInfo, error) {
		if !ok.Load() {
			return nil, errors.New("upstream 503")
		}
		return &egressInfo{IP: "1.1.1.1", Country: "US", CheckedAt: time.Now(), ViaNode: viaNode}, nil
	}

	c.GetOrRefresh("p", "http://x", "node-A")
	time.Sleep(50 * time.Millisecond)

	// Force the cache entry stale.
	c.mu.Lock()
	c.entries["p"].info.CheckedAt = time.Now().Add(-2 * egressTTL)
	c.mu.Unlock()

	ok.Store(false)
	c.GetOrRefresh("p", "http://x", "node-A")
	time.Sleep(50 * time.Millisecond)

	got := c.GetOrRefresh("p", "http://x", "node-A")
	if got == nil || got.IP != "1.1.1.1" {
		t.Fatalf("expected old cached IP preserved, got %+v", got)
	}
	if !got.Stale {
		t.Errorf("expected Stale=true after probe failure, got %+v", got)
	}
}

// TestEgress_ProbeIpinfoIntegration covers the actual JSON parser by
// pointing probeIpinfo at an httptest mock — no real proxy involved, the
// http.Client used has no Proxy transport.
func TestEgress_ProbeIpinfoIntegration(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ip":"104.21.1.1","city":"San Francisco","region":"California","country":"US","org":"Cloudflare, Inc.","timezone":"America/Los_Angeles"}`))
	}))
	defer srv.Close()

	body, err := getThrough(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var raw struct {
		IP, City, Region, Country, Org string
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw.IP != "104.21.1.1" || raw.Country != "US" || raw.City != "San Francisco" || raw.Org != "Cloudflare, Inc." {
		t.Errorf("decoded %+v", raw)
	}
}

func TestEgress_ProbeCFTraceParse(t *testing.T) {
	t.Parallel()
	body := []byte("fl=12f456\nh=www.cloudflare.com\nip=104.21.2.2\nts=1234.5\nvisit_scheme=https\nloc=US\n")
	var ip, loc string
	for _, line := range strings.Split(string(body), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "ip":
			ip = v
		case "loc":
			loc = v
		}
	}
	if ip != "104.21.2.2" || loc != "US" {
		t.Errorf("parsed ip=%q loc=%q", ip, loc)
	}
}

func TestProxyURLForPool(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"urltest-primary", "http://127.0.0.1:11082"},
		{"urltest-backup", "http://127.0.0.1:11081"},
		{"direct", ""},
		{"urltest-other", ""},
	}
	for _, c := range cases {
		got := proxyURLForPool(c.in)
		if got != c.want {
			t.Errorf("proxyURLForPool(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
