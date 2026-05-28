package subscribe

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

type fetcher struct {
	client    *http.Client
	userAgent string
}

func newFetcher(timeout time.Duration, ua string) *fetcher {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	if ua == "" {
		ua = "leap-gateway/0.1"
	}
	return &fetcher{
		client:    &http.Client{Timeout: timeout},
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
		return nil, fmt.Errorf("http %d from %s", resp.StatusCode, url)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}
