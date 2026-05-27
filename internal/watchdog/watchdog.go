// Package watchdog runs an in-process active health check on the urltest
// outbound's currently selected member. sing-box urltest only re-tests the
// pool every Interval (default 3m); a single hung node within that window
// would silently break overseas traffic. The watchdog probes the live
// selection at a faster cadence and forces a re-evaluation when the active
// node fails enough times in a row.
package watchdog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

const (
	selectorTag = "urltest"
)

// Watchdog polls clash-api on a fixed interval to verify the urltest
// selection is healthy, and triggers a forced re-evaluation when not.
type Watchdog struct {
	api      config.ClashAPIConfig
	cfg      config.WatchdogConfig
	probeURL string
	client   *http.Client
}

func New(api config.ClashAPIConfig, cfg config.WatchdogConfig, probeURL string) *Watchdog {
	httpTimeout := cfg.Timeout + 2*time.Second
	if httpTimeout < 5*time.Second {
		httpTimeout = 5 * time.Second
	}
	return &Watchdog{
		api:      api,
		cfg:      cfg,
		probeURL: probeURL,
		client:   &http.Client{Timeout: httpTimeout},
	}
}

// Run blocks until ctx is cancelled. It logs all state transitions and forced
// re-evaluations through slog so operators can correlate with sing-box logs.
func (w *Watchdog) Run(ctx context.Context) {
	if !w.cfg.Enabled {
		slog.Info("watchdog disabled by config")
		return
	}
	slog.Info("watchdog started",
		"interval", w.cfg.Interval,
		"timeout", w.cfg.Timeout,
		"fail_threshold", w.cfg.FailThreshold,
		"probe_url", w.probeURL)

	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()

	var (
		fails   int
		lastNow string
	)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now, err := w.currentSelected(ctx)
		if err != nil {
			// sing-box may be restarting (post-Reload); back off this tick.
			slog.Debug("watchdog: cannot read current selection", "err", err)
			continue
		}
		if now == "" {
			// urltest pool empty (no airport nodes yet) — nothing to watch.
			continue
		}
		if now != lastNow {
			slog.Info("watchdog: selection changed", "from", lastNow, "to", now)
			lastNow = now
			fails = 0
			continue
		}
		if err := w.probe(ctx, now); err != nil {
			fails++
			slog.Warn("watchdog: probe failed",
				"node", now, "consecutive_fails", fails, "err", err)
			if fails >= w.cfg.FailThreshold {
				slog.Warn("watchdog: triggering forced re-evaluation",
					"node", now, "after_fails", fails)
				if err := w.forceReselect(ctx); err != nil {
					slog.Error("watchdog: force re-evaluation failed", "err", err)
					// Keep failing counter so we retry on next tick.
				} else {
					fails = 0
					lastNow = "" // make next tick re-read selection
				}
			}
			continue
		}
		if fails > 0 {
			slog.Info("watchdog: probe recovered", "node", now)
		}
		fails = 0
	}
}

// currentSelected returns urltest's currently chosen member tag. Empty string
// means urltest is not yet ready (pool empty, sing-box starting).
func (w *Watchdog) currentSelected(ctx context.Context) (string, error) {
	var resp struct {
		Now string `json:"now"`
	}
	if err := w.get(ctx, "/proxies/"+selectorTag, &resp); err != nil {
		return "", err
	}
	return resp.Now, nil
}

// probe triggers a single delay measurement on the named node. clash-api
// returns 200 with {"delay": N} on success, or non-2xx / a non-empty
// "message" on failure.
func (w *Watchdog) probe(ctx context.Context, node string) error {
	timeoutMs := int(w.cfg.Timeout / time.Millisecond)
	p := fmt.Sprintf("/proxies/%s/delay?url=%s&timeout=%d",
		url.PathEscape(node), url.QueryEscape(w.probeURL), timeoutMs)
	var resp struct {
		Delay   int    `json:"delay"`
		Message string `json:"message"`
	}
	if err := w.get(ctx, p, &resp); err != nil {
		return err
	}
	if resp.Message != "" {
		return fmt.Errorf("delay test reported: %s", resp.Message)
	}
	if resp.Delay <= 0 {
		return fmt.Errorf("delay = %d (no measurement)", resp.Delay)
	}
	return nil
}

// forceReselect makes sing-box re-test every member of urltest in parallel
// and re-pick the lowest-RTT survivor. This is what /proxies/{urltest}/delay
// does internally — it propagates results back into urltest's selection
// state.
func (w *Watchdog) forceReselect(ctx context.Context) error {
	timeoutMs := int(5 * time.Second / time.Millisecond)
	p := fmt.Sprintf("/proxies/%s/delay?url=%s&timeout=%d",
		url.PathEscape(selectorTag), url.QueryEscape(w.probeURL), timeoutMs)
	// Use a longer-lived client for this — full pool tests can take 10s.
	client := &http.Client{Timeout: 30 * time.Second}
	u := "http://" + w.api.ExternalController + p
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if w.api.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+w.api.Secret)
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("clash-api urltest/delay: status %d", res.StatusCode)
	}
	return nil
}

func (w *Watchdog) get(ctx context.Context, path string, out any) error {
	u := "http://" + w.api.ExternalController + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if w.api.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+w.api.Secret)
	}
	res, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("clash-api %s: status %d", path, res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(out)
}
