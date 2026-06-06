// Package dataplane manages the proxy data-plane process (mihomo). The
// Controller exposes a small surface — health probe via clash-api, reload
// via systemctl restart — used by the API package and the subscription
// scheduler. Engine-agnostic by design: future backends (e.g. sing-box,
// xray) would slot in here, but as of 2026-06 mihomo is the only one.
//
// History: this code lived under internal/singbox/ when sing-box was the
// default engine. After the mihomo cutover the sing-box renderer was
// deleted; the controller is engine-neutral and was renamed to
// internal/dataplane/ to reflect that.
package dataplane

import (
	"context"
	"fmt"
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
// triggers a systemd restart of the configured unit (mihomo's clash-api
// PUT /configs hot-reload is used by the NodeScorer separately for pool
// changes — but whitelist/subscription rerenders go through systemctl
// because they touch more than just proxy membership).
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

// Reload restarts the data-plane systemd unit so it re-reads its config
// file. Connections drop briefly during restart; for a 30-min refresh
// cadence this is acceptable.
func (c *Controller) Reload(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "systemctl", "restart", c.SystemdUnit)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart %s: %w: %s", c.SystemdUnit, err, string(out))
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
