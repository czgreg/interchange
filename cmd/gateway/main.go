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
	"strings"
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
	"github.com/leap-gateway/leap-gateway/internal/notify"
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
	printEnv := flag.Bool("print-env", false, "print shell-sourceable env vars from gateway.yaml for use by iproute.sh / install.sh, then exit")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	// --print-env: emit shell-sourceable variables consumed by iproute.sh
	// and install.sh. Avoids fragile grep/awk YAML parsing in shell scripts
	// for values that must be exact (client_subnet, tproxy_port, api port).
	if *printEnv {
		apiPort := cfg.API.Listen
		if idx := len(apiPort) - 1; idx >= 0 {
			for i := len(apiPort) - 1; i >= 0; i-- {
				if apiPort[i] == ':' { apiPort = apiPort[i+1:]; break }
			}
		}
		fmt.Printf("export LEAP_CLIENT_SUBNET=%q\n", cfg.Node.ClientSubnet)
		fmt.Printf("export LEAP_TPROXY_PORT=%d\n", cfg.DataPlane.TProxyPort)
		fmt.Printf("export LEAP_TUN0_IFACE=tun0\n")
		fmt.Printf("export LEAP_API_PORT=%q\n", apiPort)
		// Space-separated source CIDRs allowed to reach the API port, for
		// nft.conf's input chain. Empty → loopback only (install.sh emits
		// no saddr accept rule, so only the `iif lo` rule matches).
		fmt.Printf("export LEAP_API_ALLOW_FROM=%q\n", strings.Join(cfg.API.AllowFrom, " "))
		return
	}

	// Wire fetcher through mihomo's loopback HTTP inbound so subscription
	// fetches egress through the pool instead of dialing overseas hosts
	// directly into the GFW (where they time out). mihomo resolves the
	// host and routes the CONNECT correctly.
	mgr := subscribe.NewManagerWithFetchAndProxy(
		cfg.Subscriptions, cfg.Subscribe.HTTPTimeout, cfg.Subscribe.UserAgent,
		dataplane.LeapInternalProxyURL)

	mihomoRenderer := mihomo.NewRenderer(cfg.DataPlane).
		WithNode(cfg.Node).
		WithSubscriptions(cfg.Subscriptions).
		WithPools(cfg.Pools).
		WithLoadBalance(cfg.LoadBalance.PerTerminal)
	// Manual mode: pin the per-terminal routing set to the operator-owned
	// PoolMembers list before any render runs. This protects against the
	// 10s warmup window where a subscription refresh could otherwise
	// render with the full qualified set as routing target.
	if cfg.NodeQualify.PoolMode == "manual" && len(cfg.NodeQualify.PoolMembers) > 0 {
		mihomoRenderer = mihomoRenderer.WithRoutingMembers(cfg.NodeQualify.PoolMembers)
	}
	var renderer api.Renderer = mihomoRenderer

	dpCtl := dataplane.NewController(cfg.DataPlane.ClashAPI)
	dpCtl.SystemdUnit = "leap-mihomo.service"
	store := configstore.New(*cfgPath)

	// UA auto-discovery fires when a subscription returns 5xx or 0 nodes on
	// the global UA and the alternate family (Clash↔sing-box) yields nodes.
	// We intentionally do NOT persist the discovered UA back to yaml:
	// subscriptions[] is operator-owned (see CLAUDE.md), and auto-writing it
	// caused a production incident where a transient error on the working UA
	// triggered a permanent flip to a broken one with no self-healing path.
	// The discovery still rescues the current refresh round; the log line
	// below is the signal for the operator to fix the yaml explicitly.
	mgr.WithUADiscoveryCallback(func(name, ua string) {
		slog.Info("subscribe: discovered working ua — update gateway.yaml manually if you want to persist it",
			"subscription", name, "working_ua", ua)
	})

	ni := nodeinfo.New(cfg.Node, Version, engineName)

	// whitelistexpand always shells out to sing-box CLI for `rule-set
	// decompile` (mihomo can't decompile its own .mrs format). The
	// install tarball ships /usr/local/bin/sing-box specifically for
	// this — leap-singbox.service does NOT run, sing-box is just a
	// helper binary on disk.
	//
	// WithProxy routes the .srs / domain-list fetches through mihomo's
	// loopback HTTP inbound so they egress past the GFW
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

	// Notification subsystem (Lark webhook + local JSONL audit log).
	// Always constructed so API handlers can interact with it; only
	// actually fires webhooks when cfg.Notifications.Enabled and a URL
	// is set. Secret loaded from disk if present (set via API earlier).
	larkSecret := loadLarkSecret(cfg.Notifications.Lark.SecretFile)
	notifier := notify.New(cfg.Notifications, larkSecret)
	if nodeScorer != nil {
		nodeScorer.SetEventHook(func(ev nodescorer.EmergencyEvent) {
			notifier.Emit(emergencyToNotifyEvent(ev))
		})
		nodeScorer.SetTransitionHook(func(t nodescorer.PoolTransition) {
			notifier.Emit(transitionToNotifyEvent(t))
		})
	}

	sched := subscribe.NewScheduler(cfg.Subscribe.RefreshInterval, func(ctx context.Context) error {
		return api.RunRefresh(ctx, mgr, renderer, dpCtl)
	})

	// --validate must run BEFORE any write to the production config so it
	// never clobbers the live config.yaml. It renders to a temp dir, runs
	// mihomo -t there, checks constraints, then exits.
	if *validate {
		if err := runValidate(cfg, renderer); err != nil {
			slog.Error("validation failed", "err", err)
			os.Exit(1)
		}
		slog.Info("validation passed")
		return
	}

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

	// Data-path asserter. Nil when there is no TPROXY path to assert
	// (client_subnet empty or tproxy_port 0) — /healthz then reports
	// process liveness only and labels itself as such.
	asserter := dataplane.NewAsserter(
		cfg.Node.ClientSubnet, "tun0", cfg.DataPlane.TProxyPort, cfg.DataPlane.ClashAPI)
	if asserter != nil {
		asserter.Emit = func(subject, body string, urgent bool) {
			sev := notify.SeverityInfo
			if urgent {
				sev = notify.SeverityUrgent
			}
			notifier.Emit(notify.Event{
				Time:     time.Now(),
				Severity: sev,
				Type:     "dataplane_assert",
				Subject:  subject,
				Body:     body,
			})
		}
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
		NodeScorer: nodeScorer,
		Notifier:   notifier,
		Asserter:   asserter,
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

	// Data-path assertion + self-repair, every 30s.
	//
	// Exists because of the 2026-08-12 outage: a systemd-networkd restart
	// deleted the `fwmark 0x44 lookup 101` policy rule and proxy traffic
	// was dead for 1h43m with every health signal green. leap-nft.service
	// is Type=oneshot + RemainAfterExit=yes, so it cannot re-run its own
	// ExecStart to repair the loss. See internal/dataplane/assert.go.
	if asserter != nil {
		go asserter.Run(ctx, 30*time.Second)
	} else {
		slog.Warn("dataplane: asserter disabled — no TPROXY data path in config",
			"client_subnet", cfg.Node.ClientSubnet, "tproxy_port", cfg.DataPlane.TProxyPort)
	}

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

// runValidate performs pre-deploy validation WITHOUT touching the live
// production config:
//  1. Renders the mihomo YAML into a temp directory (never writes to
//     cfg.DataPlane.ConfigPath). Uses RenderOnly so the renderer itself
//     does no I/O.
//  2. Sets SAFE_PATHS=<rule-sets dir> so mihomo -t accepts the absolute
//     rule-provider paths in the rendered YAML (they live outside the temp
//     working dir but are read-only).
//  3. Runs `mihomo -d <tempdir> -t` for full syntax + provider validation.
//  4. Checks configuration constraints (tproxy_port must be > 0,
//     pool rule_sets .mrs files must exist on disk).
//  5. Cleans up the temp dir.
//
// Called by --validate flag BEFORE any bootstrap write. Design goal: a bad
// gateway.yaml or renderer bug must never reach the live mihomo process.
func runValidate(cfg *config.Config, r api.Renderer) error {
	// 1. Render to temp dir (no writes to production).
	tmpDir, err := os.MkdirTemp("", "leap-validate-*")
	if err != nil {
		return fmt.Errorf("validate: mktemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	out, err := r.RenderOnly(nil)
	if err != nil {
		return fmt.Errorf("validate: render: %w", err)
	}
	tmpConfig := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(tmpConfig, out, 0o644); err != nil {
		return fmt.Errorf("validate: write temp config: %w", err)
	}
	slog.Info("validate: rendered to temp", "path", tmpConfig)

	// 2. mihomo's SAFE_PATHS check: rule-providers in the rendered YAML are
	//    absolute paths under cfg.DataPlane.RuleSetsDir (e.g.
	//    /var/lib/leap/mihomo/rule-sets/geosite-cn.mrs). Without this env var
	//    `mihomo -t` rejects them because they are outside the temp working
	//    dir we pass via `-d`. Granting read access to the real rule-sets dir
	//    via SAFE_PATHS avoids rewriting paths in the rendered bytes.
	safePaths := ""
	if cfg.DataPlane.RuleSetsDir != "" {
		safePaths = cfg.DataPlane.RuleSetsDir
	}

	// 3. mihomo -t: full YAML + provider-reference syntax check.
	mihomoCmd := exec.Command("/usr/local/bin/mihomo", "-d", tmpDir, "-t")
	if safePaths != "" {
		mihomoCmd.Env = append(os.Environ(), "SAFE_PATHS="+safePaths)
	}
	if cmdOut, err := mihomoCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("validate: mihomo -t failed: %w\n%s", err, cmdOut)
	}
	slog.Info("validate: mihomo -t passed")

	// 4. Configuration constraints.
	// TPROXY is the production data path. TUN-only mode collapses all
	// client sourceIPs to 198.18.0.0, which silently breaks per-terminal
	// routing, /connections metadata, and any future feature that depends
	// on real srcIP. leap-nft.service / iproute.sh also run unconditionally,
	// so a 0/unset tproxy_port would attempt to install an iptables rule
	// targeting port 0. Refuse to validate it.
	if cfg.DataPlane.TProxyPort == 0 {
		return fmt.Errorf("validate: data_plane.tproxy_port must be > 0 (TPROXY is the production path; TUN-only collapses all clients to 198.18.0.0). Set tproxy_port: 7893 in gateway.yaml")
	}
	for _, pool := range cfg.Pools {
		for _, tag := range pool.RuleSets {
			p := filepath.Join(cfg.DataPlane.RuleSetsDir, tag+".mrs")
			if _, err := os.Stat(p); err != nil {
				return fmt.Errorf("validate: pool %q rule_set %q: .mrs not found at %s — run scripts/stage.sh", pool.Name, tag, p)
			}
		}
	}

	return nil
}

// loadLarkSecret reads the Lark webhook signing secret from disk. Returns
// "" when the file is missing — Notifier handles "no secret" gracefully
// (signature_required + empty secret = fail-closed). Whitespace trimmed.
func loadLarkSecret(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(bytesTrim(data))
}

// bytesTrim trims trailing whitespace from a secret read off disk.
// `echo "secret" > file` adds a newline that breaks Lark signing.
func bytesTrim(b []byte) []byte {
	end := len(b)
	for end > 0 {
		c := b[end-1]
		if c == '\n' || c == '\r' || c == ' ' || c == '\t' {
			end--
			continue
		}
		break
	}
	return b[:end]
}

// emergencyToNotifyEvent maps a scorer EmergencyEvent into the user-facing
// notify.Event. Severity heuristics:
//   - evict / exhausted   → urgent (user impact: pool shrunk / cascade)
//   - promote             → done   (auto-recovery succeeded)
//   - clear               → info   (operator-driven)
func emergencyToNotifyEvent(ev nodescorer.EmergencyEvent) notify.Event {
	sev := notify.SeverityInfo
	subject := ev.Type
	switch ev.Type {
	case "evict", "exhausted":
		sev = notify.SeverityUrgent
		subject = "pool member auto-evicted: " + ev.Node
		if ev.Type == "exhausted" {
			subject = "emergency_promote_chain exhausted — pool shrunk"
		}
	case "promote":
		sev = notify.SeverityDone
		subject = "promoted from chain: " + ev.Node
	case "clear":
		sev = notify.SeverityInfo
		subject = "operator cleared emergency state"
	}
	return notify.Event{
		Time:     ev.At,
		Severity: sev,
		Type:     "scorer_" + ev.Type,
		Node:     ev.Node,
		Subject:  subject,
		Body:     ev.Reason,
	}
}

// transitionToNotifyEvent maps a PoolTransition into a notify.Event.
// This is what catches "机场飘" — auto K-gating decided to swap a pool
// member because EWMA scoring favored a candidate. info severity (not
// urgent: nothing's broken, system is doing its job), but ops should
// know so they can correlate with user reports.
//
// Bootstrap transitions (type="bootstrap", emitted on the first
// scoring round after a restart) are skipped — recorded for audit
// transparency but firing Lark on every redeploy is noise.
//
// Format for the Lark body uses Chinese plus arrows so the operator
// can see at a glance what changed without parsing prose:
//
//   ▼ 剔除: <node>
//   ▲ 新增: <node>
//
//   当前池 (K=8):
//     ash/🇺🇸US-IEPL-01
//     ...
//     fishcloud/🇺🇸 美国03  ← 刚加入
//
//   原因: K-gating composite-score swap
func transitionToNotifyEvent(t nodescorer.PoolTransition) notify.Event {
	if t.Type == "bootstrap" {
		return notify.Event{} // suppressed — see Notifier.Emit
	}
	subject := buildTransitionSubject(t)
	return notify.Event{
		Time:     t.At,
		Severity: notify.SeverityInfo,
		Type:     "pool_" + t.Type,
		Subject:  subject,
		Body:     buildTransitionBody(t),
	}
}

// buildTransitionSubject is the one-line headline. For 1+1 swaps it
// reads "X 换 Y"; for asymmetric / multi-node it falls back to "+N -M".
func buildTransitionSubject(t nodescorer.PoolTransition) string {
	switch {
	case len(t.Added) == 1 && len(t.Removed) == 1:
		return "池变更: " + truncate(t.Removed[0], 32) + " → " + truncate(t.Added[0], 32)
	case len(t.Added) > 0 && len(t.Removed) == 0:
		return fmt.Sprintf("池新增 %d 个节点", len(t.Added))
	case len(t.Added) == 0 && len(t.Removed) > 0:
		return fmt.Sprintf("池剔除 %d 个节点", len(t.Removed))
	}
	return fmt.Sprintf("池变更: 新增 %d 剔除 %d", len(t.Added), len(t.Removed))
}

// buildTransitionBody is the detailed multi-line body shown under the
// subject. Includes added/removed list (with arrows), full new-pool
// listing (so ops doesn't need a second curl to see "what is the pool
// now"), and metadata (source, reason).
func buildTransitionBody(t nodescorer.PoolTransition) string {
	var b strings.Builder
	if len(t.Removed) > 0 {
		b.WriteString("▼ 剔除:\n")
		for _, n := range t.Removed {
			b.WriteString("  ")
			b.WriteString(n)
			b.WriteString("\n")
		}
	}
	if len(t.Added) > 0 {
		b.WriteString("▲ 新增:\n")
		for _, n := range t.Added {
			b.WriteString("  ")
			b.WriteString(n)
			b.WriteString("\n")
		}
	}
	addedSet := map[string]bool{}
	for _, n := range t.Added {
		addedSet[n] = true
	}
	if len(t.PoolAfter) > 0 {
		fmt.Fprintf(&b, "\n当前池 (%d 个):\n", len(t.PoolAfter))
		for _, n := range t.PoolAfter {
			b.WriteString("  ")
			b.WriteString(n)
			if addedSet[n] {
				b.WriteString("  ← 新")
			}
			b.WriteString("\n")
		}
	}
	if t.Reason != "" {
		fmt.Fprintf(&b, "\n触发: %s", t.Reason)
	}
	if t.Source != "" {
		fmt.Fprintf(&b, "\n来源: %s", t.Source)
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
