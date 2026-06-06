// Command render-mihomo emits a mihomo Clash YAML config for a gateway.yaml,
// fetching subscriptions live. Useful for offline debugging / validating the
// mihomo renderer output without running the full gateway process.
//
// Usage:
//
//	render-mihomo --config /etc/leap/gateway.yaml > mihomo.yaml
//	render-mihomo --config /etc/leap/gateway.yaml --out /tmp/mihomo.yaml
//
// The rendered config does NOT replace anything — caller decides where to
// drop it. Subscriptions are fetched directly from upstream URLs (no
// HTTP-proxy detour), so this CLI works on the operator workstation as
// long as those URLs are reachable.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
	"github.com/leap-gateway/leap-gateway/internal/mihomo"
	"github.com/leap-gateway/leap-gateway/internal/subscribe"
)

func main() {
	var (
		configPath = flag.String("config", "/etc/leap/gateway.yaml", "path to gateway.yaml")
		outPath    = flag.String("out", "", "write rendered yaml here (default: stdout)")
		timeout    = flag.Duration("timeout", 60*time.Second, "subscription fetch + render budget")
	)
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		os.Exit(1)
	}

	mgr := subscribe.NewManagerWithFetch(cfg.Subscriptions, cfg.Subscribe.HTTPTimeout, cfg.Subscribe.UserAgent)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	results, err := mgr.Refresh(ctx)
	if err != nil {
		// Don't bail — partial results are still useful for a render preview.
		// We surface the error to stderr so operators see which sub failed.
		fmt.Fprintln(os.Stderr, "refresh (non-fatal):", err)
	}
	totalNodes := 0
	for _, r := range results {
		fmt.Fprintf(os.Stderr, "  %-12s nodes=%d\n", r.Name, len(r.Outbounds))
		totalNodes += len(r.Outbounds)
	}
	fmt.Fprintf(os.Stderr, "  TOTAL nodes parsed: %d\n", totalNodes)

	r := mihomo.NewRenderer(cfg.DataPlane).
		WithNode(cfg.Node).
		WithSubscriptions(cfg.Subscriptions)
	body, err := r.Write(mgr.AllOutbounds())
	if err != nil {
		fmt.Fprintln(os.Stderr, "render:", err)
		os.Exit(1)
	}

	if *outPath == "" {
		_, _ = os.Stdout.Write(body)
		return
	}
	if err := os.WriteFile(*outPath, body, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", *outPath, len(body))
}
