package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/api"
	"github.com/leap-gateway/leap-gateway/internal/config"
	"github.com/leap-gateway/leap-gateway/internal/configstore"
	"github.com/leap-gateway/leap-gateway/internal/nodeinfo"
	"github.com/leap-gateway/leap-gateway/internal/rulesets"
	"github.com/leap-gateway/leap-gateway/internal/singbox"
	"github.com/leap-gateway/leap-gateway/internal/subscribe"
	"github.com/leap-gateway/leap-gateway/internal/watchdog"
	"github.com/leap-gateway/leap-gateway/internal/whitelistexpand"
)

// Version is set at build time via -ldflags="-X main.Version=...". Defaults
// to "dev" for local builds.
var Version = "dev"

func main() {
	cfgPath := flag.String("config", "/etc/leap/gateway.yaml", "config file path")
	renderOnce := flag.Bool("render-once", false, "render bootstrap sing-box config and exit (used by install.sh to avoid first-boot fail-restart)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	mgr := subscribe.NewManagerWithFetch(cfg.Subscriptions, cfg.Subscribe.HTTPTimeout, cfg.Subscribe.UserAgent)
	renderer := singbox.NewRenderer(cfg.SingBox).
		WithNode(cfg.Node).
		WithSubscriptions(cfg.Subscriptions)
	sbCtl := singbox.NewController(cfg.SingBox.ClashAPI)
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
	wd := watchdog.New(cfg.SingBox.ClashAPI, cfg.SingBox.URLTest.Watchdog, cfg.SingBox.URLTest.ProbeURL)
	ni := nodeinfo.New(cfg.Node, Version)
	expander := whitelistexpand.New("").
		WithRuleSets(cfg.SingBox.RuleSetsDir, cfg.SingBox.BinaryPath)
	if err := expander.LoadFromDisk(); err != nil {
		slog.Warn("whitelistexpand: cannot load on-disk cache", "err", err)
	}

	ruleMgr, err := rulesets.New(cfg.SingBox.RuleSetsDir, cfg.Subscribe.HTTPTimeout, singbox.LeapInternalProxyURL)
	if err != nil {
		slog.Error("rulesets: load embedded catalog", "err", err)
		os.Exit(1)
	}

	sched := subscribe.NewScheduler(cfg.Subscribe.RefreshInterval, func(ctx context.Context) error {
		return api.RunRefresh(ctx, mgr, renderer, sbCtl)
	})

	if _, err := renderer.Write(nil); err != nil {
		slog.Error("write bootstrap singbox config", "err", err)
		os.Exit(1)
	}
	slog.Info("bootstrap singbox config written", "path", renderer.Path())

	if *renderOnce {
		return
	}

	srv := api.NewServer(api.Deps{
		Subscribe:  mgr,
		Scheduler:  sched,
		Renderer:   renderer,
		Controller: sbCtl,
		Cfg:        cfg,
		Store:      store,
		NodeInfo:   ni,
		Watchdog:   wd,
		Expander:   expander,
		RuleSets:   ruleMgr,
	})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

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
			if err := api.RunRefresh(ctx, mgr, renderer, sbCtl); err != nil {
				slog.Warn("initial refresh failed", "err", err)
			}
		}()
	}

	go sched.Run(ctx)

	go wd.Run(ctx)
	go ni.Run(ctx)

	// Best-effort fetch of the two infra rule-sets (geosite-cn / geoip-cn)
	// that route classification depends on. Their URLs come from
	// gateway.yaml singbox.route.{geosite_url,geoip_url}, not the embedded
	// catalog. No-op when the .srs is already on disk (the common case;
	// install.sh / make stage drops them there). Just a safety net for
	// freshly-deployed nodes that haven't been staged.
	go func() {
		waitFor(ctx, 3*time.Second)
		warmCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		if err := ruleMgr.EnsureInstalledFromURL(warmCtx, "geosite-cn", cfg.SingBox.Route.GeositeURL); err != nil {
			slog.Warn("rulesets: geosite-cn warm-up failed", "err", err)
		}
		if err := ruleMgr.EnsureInstalledFromURL(warmCtx, "geoip-cn", cfg.SingBox.Route.GeoIPURL); err != nil {
			slog.Warn("rulesets: geoip-cn warm-up failed", "err", err)
		}
	}()

	// Warm the resolved-snapshot cache (geosite domain expansion + geoip
	// .srs decompile + literal merge) at startup. Doing it async means
	// /api/whitelist/resolved can serve the previous on-disk cache
	// immediately; the refresh just makes it current. Wait a couple seconds
	// first to avoid contending with the initial subscription refresh +
	// sing-box reload.
	go func() {
		waitFor(ctx, 8*time.Second)
		warmCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		wl := cfg.SingBox.Route.Whitelist
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
