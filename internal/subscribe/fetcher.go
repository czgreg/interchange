package subscribe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/leaphttp"
)

// HTTPError is returned by fetcher when the upstream responded with a
// non-200 status. Lets callers (Refresh) distinguish 5xx — worth retrying
// with an alternate UA — from 4xx (auth / token issues, retry won't help).
type HTTPError struct {
	Status int
	URL    string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("http %d from %s", e.Status, e.URL)
}

// AsHTTPError unwraps err to *HTTPError if present, returning nil otherwise.
func AsHTTPError(err error) *HTTPError {
	var herr *HTTPError
	if errors.As(err, &herr) {
		return herr
	}
	return nil
}

type fetcher struct {
	httpc     *http.Client // leaphttp client: proxy-via with direct fallback
	userAgent string
}

func newFetcher(timeout time.Duration, ua string) *fetcher {
	return newFetcherWithProxy(timeout, ua, "")
}

// newFetcherWithProxy builds a fetcher that routes upstream HTTPS through
// the given HTTP proxy URL. Production use: route through the local mihomo
// loopback HTTP inbound (LeapInternalProxyURL) so subscription fetches
// egress through the pool instead of dialing overseas airport hostnames
// directly into the GFW. Without this, fetches to those hostnames time out
// — caught in production 2026-06-05 when 3 of 5 subscriptions stopped
// pulling nodes.
//
// Transport-layer failures (proxy unreachable, dial timeout) automatically
// fall back to direct dial inside leaphttp.NewClient — better to attempt a
// direct fetch and risk one failure than to lock the operator out of
// refresh entirely while the data plane is recovering. HTTP-status
// errors (4xx/5xx) do NOT trigger fallback because the upstream answered
// and a different egress path won't change its mind.
//
// Empty proxy → direct only (no fallback; primary is already direct).
func newFetcherWithProxy(timeout time.Duration, ua, proxyURL string) *fetcher {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	if ua == "" {
		ua = "leap-gateway/0.1"
	}
	return &fetcher{
		httpc:     leaphttp.NewClient(proxyURL, timeout, "subscribe"),
		userAgent: ua,
	}
}

func (f *fetcher) Get(ctx context.Context, url string) ([]byte, error) {
	return f.GetWithUA(ctx, url, "")
}

// GetWithUA fetches url using the override UA if non-empty, else the
// fetcher's default. Used by Manager.Refresh to support per-subscription
// User-Agent (some airports gate content by UA in incompatible ways).
//
// Proxy/direct fallback is handled transparently inside the leaphttp
// client; this method only converts non-200 responses to *HTTPError so
// callers can distinguish "upstream said no" from "transport failure"
// for retry decisions (see parser.go's 5xx-retry).
func (f *fetcher) GetWithUA(ctx context.Context, target, ua string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if ua == "" {
		ua = f.userAgent
	}
	req.Header.Set("User-Agent", ua)
	resp, err := f.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{Status: resp.StatusCode, URL: target}
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}
