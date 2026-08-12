// Package dataplane manages the proxy data-plane process (mihomo). The
// Controller exposes a small surface — health probe via clash-api, reload
// via clash-api PUT /configs (or systemctl restart as fallback) — used by
// the API package and the subscription scheduler. Engine-agnostic by
// design: future backends (e.g. sing-box, xray) would slot in here, but
// as of 2026-06 mihomo is the only one.
//
// History: this code lived under internal/singbox/ when sing-box was the
// default engine. After the mihomo cutover the sing-box renderer was
// deleted; the controller is engine-neutral and was renamed to
// internal/dataplane/ to reflect that.
package dataplane

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

// Loopback HTTP-proxy ports the renderer always emits for leap-gateway's
// own outbound use:
//
//   - LeapInternalProxyPort: rulesets.Manager / whitelistexpand / etc. dial
//     through this so .srs / domain-list fetches transit the airport
//     (raw.githubusercontent and friends are GFW-blocked from inside CN).
//     Behind mihomo's "out" selector → us-pool by default.
//
// LeapInternalProxyURL is the http:// URL form, ready to pass to
// http.Transport.Proxy or to leaphttp.NewClient.
const (
	LeapInternalProxyPort = 11080
	LeapInternalProxyURL  = "http://127.0.0.1:11080"
)

// Controller manages the proxy data-plane process (mihomo). Reload
// hot-reloads via clash-api PUT /configs?force=true (in-process — no
// process restart, so the listeners stay bound and leap-gateway's own API
// client / in-flight ssh sessions survive).
//
// It does NOT preserve users' established proxied connections. This
// comment claimed it did until 2026-08-12; the claim was wrong and had
// two independent measurements against it:
//   - a reload during a stress run reset the workers' in-flight
//     connections, showing up as a 23.3% ERR spike for that tier
//     (memory/stress-baseline-2026-06-07.md)
//   - each reload rebuilds the LoadBalance consistent-hash ring, which
//     breaks long-lived flows — the ChatGPT SSE stalls that motivated
//     the Plan-A time-scale split
//     (memory/nodescorer-time-scale-split.md)
//
// Treat every reload as user-visible: that is precisely why the scorer
// throttles it (hot_reload_min_interval) and why pool churn is a cost
// to be minimized rather than a free correction.
//
// Falls back to `systemctl restart` only when clash-api is unreachable —
// that path additionally drops the listeners, and on at least one node
// (89, 2026-06-07) leaked out to the host SSH session via leap-nft's
// PartOf=leap-mihomo restart chain re-running iproute.sh.
type Controller struct {
	api    config.ClashAPIConfig
	client *http.Client
	// SystemdUnit is the systemd service name to restart on Reload.
	// Defaults to "leap-mihomo.service".
	SystemdUnit string
}

func NewController(api config.ClashAPIConfig) *Controller {
	return &Controller{
		api:         api,
		client:      &http.Client{Timeout: 10 * time.Second},
		SystemdUnit: "leap-mihomo.service",
	}
}

// Reload tells the data-plane to re-read its config file at configPath.
// Tries clash-api PUT /configs?force=true first (in-process, no
// connection churn). Falls back to systemctl restart if the API is
// unreachable (cold-boot, mid-restart, etc.). configPath="" forces the
// systemctl path — used when the caller doesn't have a renderer handy.
func (c *Controller) Reload(ctx context.Context, configPath string) error {
	if configPath != "" && c.api.ExternalController != "" {
		if err := c.hotReload(ctx, configPath); err == nil {
			return nil
		} else {
			slog.Warn("dataplane: clash-api hot-reload failed, falling back to systemctl restart",
				"err", err, "path", configPath)
		}
	}
	cmd := exec.CommandContext(ctx, "systemctl", "restart", c.SystemdUnit)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart %s: %w: %s", c.SystemdUnit, err, string(out))
	}
	return nil
}

// hotReload PUTs to mihomo's clash-api /configs?force=true endpoint with
// the on-disk config path. force=true tells mihomo to drop its current
// state and load the new file; established proxy flows are kept where
// possible. Returns error so caller can fall back to systemctl restart.
func (c *Controller) hotReload(ctx context.Context, configPath string) error {
	body := []byte(`{"path":"` + configPath + `"}`)
	url := "http://" + c.api.ExternalController + "/configs?force=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.api.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.api.Secret)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("clash-api PUT /configs status %d: %s", resp.StatusCode, b)
	}
	return nil
}

// Health pings the data plane's clash API; useful for early failure
// detection. Surfaces as `engine_ok` in /api/status.
func (c *Controller) Health(ctx context.Context) error {
	url := fmt.Sprintf("http://%s/version", c.api.ExternalController)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if c.api.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.api.Secret)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("clash api status %d", resp.StatusCode)
	}
	return nil
}
