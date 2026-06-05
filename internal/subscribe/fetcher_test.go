package subscribe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestFetcher_ProxyFallback ensures that when the configured proxy is
// unreachable (data plane down / mid-restart), the fetcher falls back to
// direct OS network instead of failing the whole refresh. Mirrors the
// production reasoning: a temporarily-down mihomo shouldn't lock operators
// out of subscription refresh.
func TestFetcher_ProxyFallback(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("OK-BODY"))
	}))
	defer srv.Close()

	// 127.0.0.1:1 is reserved (tcpmux) and almost certainly closed —
	// http.Client through that proxy will fail with connection refused.
	f := newFetcherWithProxy(2*time.Second, "test", "http://127.0.0.1:1")
	body, err := f.GetWithUA(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("expected fallback to direct, got err: %v", err)
	}
	if string(body) != "OK-BODY" {
		t.Errorf("body = %q, want OK-BODY", body)
	}
}

// TestFetcher_NoFallbackOnHTTPError ensures HTTPError (4xx/5xx) doesn't
// trigger the fallback — those are upstream rejections that don't depend
// on egress path.
func TestFetcher_NoFallbackOnHTTPError(t *testing.T) {
	t.Parallel()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		http.Error(w, "auth fail", http.StatusForbidden)
	}))
	defer srv.Close()

	// Use srv.URL itself as the "proxy" so it accepts the CONNECT/GET and
	// returns 403 — exercising the HTTPError-but-reachable path.
	f := newFetcherWithProxy(2*time.Second, "test", srv.URL)
	_, err := f.GetWithUA(context.Background(), srv.URL, "")
	if err == nil {
		t.Fatal("expected HTTPError, got nil")
	}
	if h := AsHTTPError(err); h == nil || h.Status != 403 {
		t.Errorf("err = %v, want HTTPError 403", err)
	}
	// Single hit only — fallback must not have fired.
	if hits != 1 {
		t.Errorf("primary was called %d times; fallback should NOT fire on HTTPError", hits)
	}
}

// TestFetcher_NoFallbackWithoutProxy ensures direct-only fetchers don't
// retry themselves (no fallback configured).
func TestFetcher_NoFallbackWithoutProxy(t *testing.T) {
	t.Parallel()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		http.Error(w, "boom", 503)
	}))
	defer srv.Close()
	f := newFetcher(2*time.Second, "test")
	_, err := f.GetWithUA(context.Background(), srv.URL, "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if hits != 1 {
		t.Errorf("hits = %d, want 1 (no fallback configured)", hits)
	}
}
