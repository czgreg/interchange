package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/api"
	"github.com/leap-gateway/leap-gateway/internal/config"
	"github.com/leap-gateway/leap-gateway/internal/configstore"
	"github.com/leap-gateway/leap-gateway/internal/dataplane"
	"github.com/leap-gateway/leap-gateway/internal/dnspreload"
	"github.com/leap-gateway/leap-gateway/internal/mihomo"
	"github.com/leap-gateway/leap-gateway/internal/nodescorer"
	"github.com/leap-gateway/leap-gateway/internal/nodeinfo"
	"github.com/leap-gateway/leap-gateway/internal/rulesets"
	"github.com/leap-gateway/leap-gateway/internal/subscribe"
	"github.com/leap-gateway/leap-gateway/internal/whitelistexpand"
)

// Version is set at build time via -ldflags="-X main.Version=...". Defaults
// to "dev" for local builds.
var Version = "dev"

// engineName is the active data-plane name — only "mihomo" is supported as
// of 2026-06. Surfaced via /api/proxies/active.leap.engine for ops.
const engineName = "mihomo"

func main() {
	cfgPath := flag.String("config", "/etc/leap/gateway.yaml", "config file path")
	renderOnce := flag.Bool("render-once", false, "render bootstrap mihomo config and exit (used by install.sh to avoid first-boot fail-restart)")
	validate := flag.Bool("validate", false, "render config, run mihomo -t syntax check, validate constraints, then exit")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	// Wire fetcher through mihomo's loopback HTTP inbound so subscription
	// fetches bypass the host resolver (fakeip mode hands out 198.18.x.x
	// for non-CN domains; direct dial fails). mihomo answers DNS via
	// cn-doh and routes the CONNECT correctly.
	mgr := subscribe.NewManagerWithFetchAndProxy(
		cfg.Subscriptions, cfg.Subscribe.HTTPTimeout, cfg.Subscribe.UserAgent,
		dataplane.LeapInternalProxyURL)

	mihomoRenderer := mihomo.NewRenderer(cfg.DataPlane).
		WithNode(cfg.Node).
		WithSubscriptions(cfg.Subscriptions).
		WithPools(cfg.Pools).
		WithLoadBalance(cfg.LoadBalance.PerTerminal)
	var renderer api.Renderer = mihomoRenderer

	dpCtl := dataplane.NewController(cfg.DataPlane.ClashAPI)
	dpCtl.SystemdUnit = "leap-mihomo.service"
	store := configstore.New(*cfgPath)

	// Persist UA auto-discoveries: when Refresh's fallback finds a working
	// per-subscription UA (Clash↔sing-box swap), write it back into yaml so
	// the next refresh hits the right UA on the first try.
	mgr.WithUADiscoveryCallback(func(name, ua string) {
		err := store.Mutate(cfg, func(c *config.Config) error {
			for i := range c.Subscriptions {
				if c.Subscriptions[i].Name == name {
					c.Subscriptions[i].UserAgent = ua
					return nil
				}
			}
			return nil
		})
		if err != nil {
			slog.Warn("subscribe: persist discovered ua failed", "name", name, "ua", ua, "err", err)
			return
		}
		mgr.SetEntries(cfg.Subscriptions)
	})

	ni := nodeinfo.New(cfg.Node, Version, engineName)

	// whitelistexpand always shells out to sing-box CLI for `rule-set
	// decompile` (mihomo can't decompile its own .mrs format). The
	// install tarball ships /usr/local/bin/sing-box specifically for
	// this — leap-singbox.service does NOT run, sing-box is just a
	// helper binary on disk.
	//
	// WithProxy routes the .srs / domain-list fetches through mihomo's
	// loopback HTTP inbound, avoiding host-resolver fakeip pollution
	// (see internal/leaphttp). Direct fallback handles the bootstrap
	// window before mihomo is ready.
	const singboxCLI = "/usr/local/bin/sing-box"
	expander := whitelistexpand.New("").
		WithProxy(dataplane.LeapInternalProxyURL).
		WithRuleSets(cfg.DataPlane.RuleSetsDir, singboxCLI).
		WithMihomoSrsCache("/var/lib/leap/whitelist-srs-cache")
	if err := expander.LoadFromDisk(); err != nil {
		slog.Warn("whitelistexpand: cannot load on-disk cache", "err", err)
	}

	ruleMgr, err := rulesets.New(cfg.DataPlane.RuleSetsDir, cfg.Subscribe.HTTPTimeout, dataplane.LeapInternalProxyURL)
	if err != nil {
		slog.Error("rulesets: load embedded catalog", "err", err)
		os.Exit(1)
	}
	ruleMgr.WithEngine(engineName)

	// NodeScorer: dynamic pool management. Scores every node on its probe
	// history + passive /connections throughput; keeps only qualified nodes
	// in us-pool; hot-reloads mihomo via PUT /configs when the set changes
	// (no systemctl restart).
	var nodeScorer *nodescorer.Scorer
	if cfg.NodeQualify.Enabled {
		ns := nodescorer.New(
			cfg.NodeQualify,
			cfg.Pools,
			cfg.DataPlane.ClashAPI.ExternalController,
			cfg.DataPlane.ClashAPI.Secret,
			cfg.DataPlane.URLTest.ProbeURL,
			cfg.DataPlane.URLTest.NodePattern,
			mihomoRenderer,
			mgr.AllOutbounds,
		)
		nodeScorer = ns
		slog.Info("nodescorer: enabled",
			"interval", cfg.NodeQualify.ScoringInterval,
			"max_rtt_p50", cfg.NodeQualify.MaxRTTP50Ms,
		)
	}

	sched := subscribe.NewScheduler(cfg.Subscribe.RefreshInterval, func(ctx context.Context) error {
		return api.RunRefresh(ctx, mgr, renderer, dpCtl)
	})

	// Bootstrap: write an initial mihomo config so the data plane can start
	// before the first subscription refresh completes.
	//
	// ONLY write if the config file does not yet exist. On subsequent
	// restarts the file already contains a valid config (last-known-good
	// state from the previous run); overwriting it with an empty-pool
	// bootstrap would:
	//   - briefly break traffic for clients already using the proxy
	//   - leave the node in a degraded state if the subscription refresh
	//     fails (e.g. airport temporarily unreachable)
	//
	// --render-once bypasses this check: it is used by install.sh on first
	// deploy when no config exists yet, and always needs a fresh write.
	if _, statErr := os.Stat(renderer.Path()); os.IsNotExist(statErr) || *renderOnce {
		if _, err := renderer.Write(nil); err != nil {
			slog.Error("write bootstrap mihomo config", "err", err)
			os.Exit(1)
		}
		slog.Info("bootstrap mihomo config written", "path", renderer.Path())
	} else {
		slog.Info("mihomo config exists — skipping bootstrap write", "path", renderer.Path())
	}

	if *renderOnce {
		return
	}

	// --validate: render-once + mihomo -t syntax check + constraint validation.
	// Exits 0 on success, non-zero on any failure. Designed to be run from
	// deploy.sh and CI before restarting mihomo.
	if *validate {
		if err := runValidate(cfg, renderer); err != nil {
			slog.Error("validation failed", "err", err)
			os.Exit(1)
		}
		slog.Info("validation passed")
		return
	}

	srv := api.NewServer(api.Deps{
		Subscribe:  mgr,
		Scheduler:  sched,
		Renderer:   renderer,
		Controller: dpCtl,
		Cfg:        cfg,
		Store:      store,
		NodeInfo:   ni,
		Expander:   expander,
		RuleSets:   ruleMgr,
		// In-memory ring of client-reported UX events. 5000 ≈ a few hours
		// of busy ops; beyond that the ring overwrites oldest. Operators
		// store if they want history; we don't try to be that store.
		NodeScorer: nodeScorer,
	})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Launch NodeScorer now that ctx is available and mihomo is up
	// (bootstrap render ran above). The 10s warm-up in scorer.Run gives
	// mihomo time to complete its first url-test round before scoring
	// begins.
	if nodeScorer != nil {
		go nodeScorer.Run(ctx)
	}

	go func() {
		slog.Info("api listening", "addr", cfg.API.Listen)
		if err := srv.ListenAndServe(ctx); err != nil {
			slog.Error("api server failed (fatal)", "err", err)
			os.Exit(1)
		}
	}()

	if cfg.Subscribe.RefreshOnStartup {
		go func() {
			waitFor(ctx, 5*time.Second)
			if err := api.RunRefresh(ctx, mgr, renderer, dpCtl); err != nil {
				slog.Warn("initial refresh failed", "err", err)
			}
		}()
	}

	go sched.Run(ctx)
	go ni.Run(ctx)

	// DNS preloader: keeps overseas-bound DNS records warm in mihomo's
	// DNS cache so the first FeiLian client to access claude.ai (or any
	// configured domain) doesn't pay the ~400ms cross-border DoH cold-
	// resolve cost. Disabled when preload_domains is empty.
	if dnsAddr := dnsListenAddr(cfg); dnsAddr != "" {
		if pre := dnspreload.New(dnsAddr, cfg.DataPlane.DNS.PreloadDomains, cfg.DataPlane.DNS.PreloadInterval); pre != nil {
			go pre.Run(ctx)
		}
	}

	// Best-effort fetch of the two infra rule-sets (geosite-cn / geoip-cn)
	// that route classification depends on. Their URLs come from
	// gateway.yaml data_plane.route.{geosite_url,geoip_url}, not the
	// embedded catalog. No-op when the .mrs is already on disk (the common
	// case; install.sh / make stage drops them there). Just a safety net
	// for freshly-deployed nodes that haven't been staged.
	go func() {
		waitFor(ctx, 3*time.Second)
		warmCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		if err := ruleMgr.EnsureInstalledFromURL(warmCtx, "geosite-cn", cfg.DataPlane.Route.GeositeURL); err != nil {
			slog.Warn("rulesets: geosite-cn warm-up failed", "err", err)
		}
		if err := ruleMgr.EnsureInstalledFromURL(warmCtx, "geoip-cn", cfg.DataPlane.Route.GeoIPURL); err != nil {
			slog.Warn("rulesets: geoip-cn warm-up failed", "err", err)
		}
	}()

	// Warm the resolved-snapshot cache (geosite domain expansion + geoip
	// .srs decompile + literal merge) at startup. Doing it async means
	// /api/whitelist/resolved can serve the previous on-disk cache
	// immediately; the refresh just makes it current. Wait a couple
	// seconds first to avoid contending with the initial subscription
	// refresh + mihomo reload.
	go func() {
		waitFor(ctx, 8*time.Second)
		warmCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		wl := cfg.DataPlane.Route.Whitelist
		if _, err := expander.Refresh(warmCtx, wl.Geosites, wl.Geoips, wl.DomainSuffix, wl.IPCIDR); err != nil {
			slog.Warn("whitelistexpand: startup warm failed", "err", err)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
}

func waitFor(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// dnsListenAddr returns the host:port that mihomo's DNS server is bound
// to. Honors an explicit DNS.Listen override; otherwise falls back to
// "<tun0_gateway_ip>:53" — the default the renderer wires up. Empty
// return means we have no idea where DNS is listening (no Tun0GatewayIP,
// no override) — caller should skip the preloader.
func dnsListenAddr(cfg *config.Config) string {
	if v := cfg.DataPlane.DNS.Listen; v != "" {
		return v
	}
	if ip := cfg.Node.Tun0GatewayIP; ip != "" {
		return ip + ":53"
	}
	return ""
}

// runValidate performs pre-deploy validation:
//  1. Renders a fresh mihomo config from the current gateway.yaml.
//  2. Runs `mihomo -d <workdir> -t` for YAML syntax validation.
//  3. Checks configuration constraints (per_terminal requires tproxy_port,
//     pool rule_sets .mrs files must exist on disk).
//
// Called by --validate flag. Designed to be invoked from deploy.sh before
// restarting mihomo, surfacing bad configs before they reach production.
func runValidate(cfg *config.Config, r api.Renderer) error {
	// 1. Render the config (writes the current config.yaml from subscriptions
	//    already in memory — nil renders a bootstrap with no nodes, which is
	//    enough to validate structure and pool/rule references).
	if _, err := r.Write(nil); err != nil {
		return fmt.Errorf("render: %w", err)
	}
	slog.Info("validate: rendered config", "path", r.Path())

	// 2. mihomo -t: syntax + provider-reference check.
	workdir := filepath.Dir(r.Path())
	mihomoCmd := exec.Command("/usr/local/bin/mihomo", "-d", workdir, "-t")
	if out, err := mihomoCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mihomo -t failed: %w\n%s", err, out)
	}
	slog.Info("validate: mihomo -t passed")

	// 3. Configuration constraints.
	if cfg.LoadBalance.PerTerminal && cfg.DataPlane.TProxyPort == 0 {
		return fmt.Errorf("load_balance.per_terminal=true requires data_plane.tproxy_port to be set (TPROXY preserves real client sourceIP; TUN mode collapses all clients to 198.18.0.0)")
	}

	// 4. Pool rule_sets .mrs files must exist.
	for _, pool := range cfg.Pools {
		for _, tag := range pool.RuleSets {
			path := filepath.Join(cfg.DataPlane.RuleSetsDir, tag+".mrs")
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("pool %q rule_set %q: .mrs not found at %s — run scripts/stage.sh to pre-fetch", pool.Name, tag, path)
			}
		}
	}

	return nil
}
