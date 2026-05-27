package singbox

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

// Controller manages the sing-box data-plane process. Reload triggers a
// systemd restart of leap-singbox.service (sing-box has no in-process config
// reload — its clash-api PUT /configs is a no-op stub when started with `-c`).
// The HTTP client is retained for clash-api queries (Health, /proxies, etc).
type Controller struct {
	api    config.ClashAPIConfig
	client *http.Client
	// SystemdUnit is the systemd service name to restart on Reload. Defaults
	// to "leap-singbox.service".
	SystemdUnit string
}

func NewController(api config.ClashAPIConfig) *Controller {
	return &Controller{
		api:         api,
		client:      &http.Client{Timeout: 10 * time.Second},
		SystemdUnit: "leap-singbox.service",
	}
}

// Reload restarts the sing-box systemd unit so it re-reads its config file.
// Connections drop briefly during restart; for a 30-min refresh cadence this
// is acceptable.
func (c *Controller) Reload(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "systemctl", "restart", c.SystemdUnit)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart %s: %w: %s", c.SystemdUnit, err, string(out))
	}
	return nil
}

// Health pings sing-box clash API; useful for early failure detection.
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
