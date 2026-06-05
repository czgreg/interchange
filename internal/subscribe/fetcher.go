package subscribe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
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
	client    *http.Client
	userAgent string
}

func newFetcher(timeout time.Duration, ua string) *fetcher {
	return newFetcherWithProxy(timeout, ua, "")
}

// newFetcherWithProxy builds a fetcher that routes upstream HTTPS through
// the given HTTP proxy URL. Production use: route through the local mihomo /
// sing-box loopback HTTP inbound (LeapInternalProxyURL) so subscription
// fetches don't try to dial fakeip-tainted upstream addresses returned by
// the host's resolver. Without this, fetches to randomly-named airport
// hostnames timeout against 198.18.x.x — caught in production 2026-06-05
// when 3 of 5 subscriptions stopped pulling nodes.
//
// Empty proxy → direct OS network (used by tests).
func newFetcherWithProxy(timeout time.Duration, ua, proxyURL string) *fetcher {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	if ua == "" {
		ua = "leap-gateway/0.1"
	}
	transport := &http.Transport{}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	return &fetcher{
		client:    &http.Client{Timeout: timeout, Transport: transport},
		userAgent: ua,
	}
}

func (f *fetcher) Get(ctx context.Context, url string) ([]byte, error) {
	return f.GetWithUA(ctx, url, "")
}

// GetWithUA fetches url using the override UA if non-empty, else the
// fetcher's default. Used by Manager.Refresh to support per-subscription
// User-Agent (some airports gate content by UA in incompatible ways).
func (f *fetcher) GetWithUA(ctx context.Context, url, ua string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if ua == "" {
		ua = f.userAgent
	}
	req.Header.Set("User-Agent", ua)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{Status: resp.StatusCode, URL: url}
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}
