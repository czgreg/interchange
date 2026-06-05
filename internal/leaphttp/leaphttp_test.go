package leaphttp

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hitCount is a tiny counter wrapping an http.Handler so tests can assert
// "primary called N times, fallback called M times" without rolling their
// own atomic-int dance.
func handlerWithCount(h http.Handler) (*atomic.Int32, http.Handler) {
	var n atomic.Int32
	return &n, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		h.ServeHTTP(w, r)
	})
}

// TestNewClient_NoProxy_DirectOnly: empty proxyURL → single direct client,
// no fallback machinery. Sanity check + lock the degraded mode.
func TestNewClient_NoProxy_DirectOnly(t *testing.T) {
	cnt, h := handlerWithCount(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := NewClient("", time.Second, "test-direct")
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if cnt.Load() != 1 {
		t.Errorf("server hit %d times, want 1", cnt.Load())
	}
}

// TestNewClient_ProxyOK_NoFallback: working proxy, 200 from upstream — only
// the proxy path runs, fallback never fires.
func TestNewClient_ProxyOK_NoFallback(t *testing.T) {
	upCnt, upH := handlerWithCount(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "via-proxy")
	}))
	upstream := httptest.NewServer(upH)
	defer upstream.Close()

	// httptest.NewServer is itself an HTTP "proxy" if we configure the
	// client's proxyURL to it AND the request's full URL is rewritten to
	// upstream — the simplest is to run a real CONNECT-style proxy. For
	// this test we use a forwarding proxy that just round-trips the
	// request to upstream regardless of req.URL.
	proxyCnt, proxyH := handlerWithCount(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate proxy forwarding: dial upstream, copy back.
		// Real http.Transport with Proxy: ProxyURL(u) speaks HTTP-proxy
		// protocol, so the proxy sees the full URL. We just return a
		// canned response — close enough for "did the proxy path run".
		_ = r
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "via-proxy")
	}))
	proxy := httptest.NewServer(proxyH)
	defer proxy.Close()

	c := NewClient(proxy.URL, time.Second, "test-proxy-ok")
	resp, err := c.Get("http://upstream.invalid/x") // host doesn't matter — proxy intercepts
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "via-proxy" {
		t.Errorf("body = %q, want via-proxy", body)
	}
	if proxyCnt.Load() != 1 {
		t.Errorf("proxy hit %d times, want 1", proxyCnt.Load())
	}
	if upCnt.Load() != 0 {
		t.Errorf("upstream hit directly %d times, want 0 (proxy should have absorbed)", upCnt.Load())
	}
}

// TestNewClient_ProxyDown_FallbackFires: proxy port is closed, primary
// transport fails to dial it, fallback retries via direct and succeeds.
// This is the bootstrap / data-plane-crash path.
func TestNewClient_ProxyDown_FallbackFires(t *testing.T) {
	upCnt, upH := handlerWithCount(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "direct-ok")
	}))
	upstream := httptest.NewServer(upH)
	defer upstream.Close()

	// Reserve a port and immediately close it — guaranteed to refuse
	// connections without flapping a real listener up and down.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadProxy := "http://" + l.Addr().String()
	_ = l.Close()

	c := NewClient(deadProxy, 2*time.Second, "test-fallback")
	resp, err := c.Get(upstream.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "direct-ok" {
		t.Errorf("body = %q, want direct-ok (fallback didn't execute)", body)
	}
	if upCnt.Load() != 1 {
		t.Errorf("upstream hit %d times, want 1 (fallback should have called once)", upCnt.Load())
	}
}

// TestNewClient_ProxyOK_HTTPError_NoFallback: proxy reaches upstream, but
// upstream answers 5xx. The HTTP status is the upstream's word; a fallback
// retry is meaningless and would just double the load. Confirm the response
// comes back as-is and fallback never fires.
func TestNewClient_ProxyOK_HTTPError_NoFallback(t *testing.T) {
	proxyCnt, proxyH := handlerWithCount(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	proxy := httptest.NewServer(proxyH)
	defer proxy.Close()

	upCnt, upH := handlerWithCount(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "would-be-direct")
	}))
	upstream := httptest.NewServer(upH)
	defer upstream.Close()

	c := NewClient(proxy.URL, time.Second, "test-http-err")
	resp, err := c.Get(upstream.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500 (proxy's response)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "boom") {
		t.Errorf("body = %q, want to contain 'boom'", body)
	}
	if proxyCnt.Load() != 1 {
		t.Errorf("proxy hit %d, want 1", proxyCnt.Load())
	}
	if upCnt.Load() != 0 {
		t.Errorf("upstream hit %d, want 0 (HTTP error must NOT fall back)", upCnt.Load())
	}
}

// TestNewClient_BadProxyURL_DegradeToDirect: malformed proxyURL is treated
// like empty — degrade to direct rather than panic. Defensive check.
func TestNewClient_BadProxyURL_DegradeToDirect(t *testing.T) {
	cnt, h := handlerWithCount(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := NewClient("://not-a-url", time.Second, "test-bad-url")
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || cnt.Load() != 1 {
		t.Errorf("expected 200 with 1 server hit, got status=%d hits=%d", resp.StatusCode, cnt.Load())
	}
}
