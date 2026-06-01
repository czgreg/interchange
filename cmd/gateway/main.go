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
	wd := watchdog.New(cfg.SingBox.ClashAPI, cfg.SingBox.URLTest.Watchdog, cfg.SingBox.URLTest.ProbeURL)
	ni := nodeinfo.New(cfg.Node, Version)
	expander := whitelistexpand.New("").
		WithRuleSets(cfg.SingBox.RuleSetsDir, cfg.SingBox.BinaryPath)
	if err := expander.LoadFromDisk(); err != nil {
		slog.Warn("whitelistexpand: cannot load on-disk cache", "err", err)
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
