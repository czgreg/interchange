package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
	"github.com/leap-gateway/leap-gateway/internal/configstore"
	"github.com/leap-gateway/leap-gateway/internal/dataplane"
	"github.com/leap-gateway/leap-gateway/internal/nodescorer"
	"github.com/leap-gateway/leap-gateway/internal/nodeinfo"
	"github.com/leap-gateway/leap-gateway/internal/notify"
	"github.com/leap-gateway/leap-gateway/internal/rulesets"
	"github.com/leap-gateway/leap-gateway/internal/subscribe"
	"github.com/leap-gateway/leap-gateway/internal/whitelistexpand"
)

type Deps struct {
	Subscribe  *subscribe.Manager
	Scheduler  *subscribe.Scheduler
	Renderer   Renderer
	Controller *dataplane.Controller
	Cfg        *config.Config
	Store      *configstore.Store
	NodeInfo   *nodeinfo.Reporter
	Expander   *whitelistexpand.Expander
	RuleSets   *rulesets.Manager
	// NodeScorer is non-nil only when NodeQualify.Enabled=true (the
	// default under mihomo). Powers /api/nodes/health and the pool-
	// summary fields in /api/status.
	NodeScorer *nodescorer.Scorer
	// Notifier carries operator-facing alerts (Lark webhook + local
	// JSONL log). Always non-nil — even when notifications.enabled=false,
	// API handlers can read status / recent log entries.
	Notifier *notify.Notifier
}

// Renderer is the interface internal/mihomo.Renderer satisfies. Kept as
// an interface (rather than collapsing to *mihomo.Renderer directly) so
// API handlers stay decoupled from the rendering package — and so a
// future second engine can slot in without touching every call site.
type Renderer interface {
	// Path returns the on-disk path the next Write produces.
	Path() string
	// Write renders a complete engine config from the given outbounds and
	// writes it to disk. Returns the rendered bytes for inspection.
	Write(outbounds []subscribe.Outbound) ([]byte, error)
	// RenderOnly renders the config and returns the bytes WITHOUT writing to
	// disk. Used by --validate to avoid touching the live production config.
	RenderOnly(outbounds []subscribe.Outbound) ([]byte, error)
	// SetSubscriptions replaces the captured subscription order in place.
	// Used by API handlers that mutate cfg and need a re-render.
	SetSubscriptions(subs []config.SubscriptionEntry)
	// SetWhitelist re-seats the renderer's snapshot of route mode + WL.
	// The renderer holds cfg by value, so the live cfg pointer's
	// mutations don't propagate automatically. Handlers that mutate the
	// whitelist must call this before triggering a re-render — otherwise
	// rendered config is stale (rule-providers / rules drop new tags).
	SetWhitelist(mode string, wl config.WhitelistConfig)
	// SetFakeIPSkip re-seats the DNS intranet skip-suffix list.
	// Synced on every PUT /api/whitelist so the renderer emits the updated
	// nameserver-policy without a separate config reload.
	SetFakeIPSkip(suffixes []string)
	// AssignmentForIP returns the HRW-ordered routing members assigned to
	// one terminal IP. ([primary, secondary, ...], true) when the IP is
	// inside the configured client_subnet AND the routing pool is non-empty,
	// (nil, false) otherwise. Used by /api/pool/terminal.
	AssignmentForIP(ip string, outbounds []subscribe.Outbound) ([]string, bool)
}

type Server struct {
	deps    Deps
	srv     *http.Server
	egress  *egressCache
}

func NewServer(deps Deps) *Server {
	mux := http.NewServeMux()
	s := &Server{deps: deps, egress: newEgressCache()}
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/status", s.auth(s.handleStatus))
	mux.HandleFunc("GET /api/nodes", s.auth(s.handleNodes))
	mux.HandleFunc("POST /api/subscribe/refresh", s.auth(s.handleRefresh))
	mux.HandleFunc("GET /api/subscribe/refresh-interval", s.auth(s.handleRefreshIntervalGet))
	mux.HandleFunc("PUT /api/subscribe/refresh-interval", s.auth(s.handleRefreshIntervalPut))

	mux.HandleFunc("GET /api/proxies/active", s.auth(s.handleProxiesActive))
	mux.HandleFunc("POST /api/proxies/select", s.auth(s.handleProxiesSelect))

	mux.HandleFunc("GET /api/whitelist", s.auth(s.handleWhitelistGet))
	mux.HandleFunc("PUT /api/whitelist", s.auth(s.handleWhitelistPut))
	mux.HandleFunc("GET /api/whitelist/resolved", s.auth(s.handleWhitelistResolved))

	mux.HandleFunc("GET /api/rule-sets", s.auth(s.handleRuleSetsGet))

	mux.HandleFunc("GET /api/subscriptions", s.auth(s.handleSubscriptionsGet))
	mux.HandleFunc("POST /api/subscriptions", s.auth(s.handleSubscriptionsPost))
	mux.HandleFunc("PUT /api/subscriptions/{name}", s.auth(s.handleSubscriptionsPut))
	mux.HandleFunc("DELETE /api/subscriptions/{name}", s.auth(s.handleSubscriptionsDelete))


	// Node health scoring — mihomo only, shows per-node RTT stats +
	// passive throughput + qualified/in-pool state.
	mux.HandleFunc("GET /api/nodes/health", s.auth(s.handleNodesHealth))

	// Manual-mode pool surfaces removed: /api/pool/state and
	// /api/pool/clear-emergency assumed pool_mode=manual semantics. With
	// auto mode the canonical entry points are /api/status.pool_sizing
	// (current K), /api/nodes/health (current pool by in_pool:true), and
	// /api/pool/transitions (audit log).
	mux.HandleFunc("GET /api/pool/terminal", s.auth(s.handlePoolTerminal))
	mux.HandleFunc("GET /api/pool/transitions", s.auth(s.handlePoolTransitions))
	mux.HandleFunc("POST /api/pool/rollback", s.auth(s.handlePoolRollback))

	// Notification subsystem — Lark webhook configuration + local log
	// inspection + ad-hoc test send.
	mux.HandleFunc("GET /api/notifications/status", s.auth(s.handleNotificationsStatus))
	mux.HandleFunc("POST /api/notifications/lark", s.auth(s.handleNotificationsLark))
	mux.HandleFunc("DELETE /api/notifications/lark", s.auth(s.handleNotificationsLarkDelete))
	mux.HandleFunc("POST /api/notifications/test", s.auth(s.handleNotificationsTest))
	mux.HandleFunc("GET /api/notifications/recent", s.auth(s.handleNotificationsRecent))

	s.srv = &http.Server{
		Addr:              deps.Cfg.API.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
	}()
	if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	tok := s.deps.Cfg.API.Token
	if tok == "" {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+tok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	results, last := s.deps.Subscribe.Snapshot()
	count := 0
	for _, r := range results {
		count += len(r.Outbounds)
	}
	engineOK := s.deps.Controller.Health(r.Context()) == nil
	resp := map[string]any{
		"last_refresh":  last,
		"node_count":    count,
		"subscriptions": len(results),
		"engine_ok":     engineOK,
	}
	// Expose NodeScorer pool summary when active (engine=mihomo).
	if s.deps.NodeScorer != nil {
		snap := s.deps.NodeScorer.GetSnapshot()
		resp["pool_qualified"] = snap.Qualified
		resp["pool_total"] = snap.Total
		resp["pool_last_update"] = snap.LastPoolUpdate
		// K-gating diagnostics. KTarget=0 means K-gating disabled (legacy
		// "every qualified candidate in pool" mode); the block is still
		// surfaced so operators can confirm config wiring.
		resp["pool_sizing"] = snap.PoolSizing
		// 24h pool churn metric — answers "is the system thrashing?"
		// without scrolling /api/pool/transitions.
		resp["pool_stability_24h"] = s.deps.NodeScorer.GetStability()
	}
	// Static capacity ceiling from the last offline stress test (see
	// scripts/stress.sh + cfg.capacity). Only surfaced when configured —
	// absent block = nobody has stress-tested this deployment yet.
	if c := s.deps.Cfg.Capacity; c.SustainedMaxUsers > 0 || c.DegradedMaxUsers > 0 {
		resp["capacity"] = map[string]any{
			"sustained_max_users": c.SustainedMaxUsers,
			"degraded_max_users":  c.DegradedMaxUsers,
			"measured_at":         c.MeasuredAt,
			"measured_with":       c.MeasuredWith,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleNodes(w http.ResponseWriter, _ *http.Request) {
	results, _ := s.deps.Subscribe.Snapshot()
	type nodeView struct {
		Tag    string `json:"tag"`
		Type   string `json:"type"`
		Server string `json:"server"`
		Source string `json:"source"`
	}
	view := []nodeView{}
	for _, r := range results {
		for _, o := range r.Outbounds {
			srv, _ := o["server"].(string)
			view = append(view, nodeView{
				Tag:    o.Tag(),
				Type:   o.Type(),
				Server: srv,
				Source: r.Name,
			})
		}
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if err := RunRefresh(r.Context(), s.deps.Subscribe, s.deps.Renderer, s.deps.Controller); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// RunRefresh is the full subscribe → render → reload flow, exposed so main()
// can also trigger it on startup or on a timer.
//
// Failures inside Refresh (per-subscription fetch / parse errors, transient
// airport RSTs, UA mismatches, even all-subs-zero outcomes) are warn-only.
// Reasons:
//   - Mutation handlers (POST/PUT/DELETE /api/subscriptions) call RunRefresh
//     AFTER the on-disk yaml has already been persisted. Reporting 5xx because
//     the post-mutate refresh hit transient errors makes the API caller think
//     the operation failed when it actually succeeded — observed during DELETE
//     on 2026-06-07 when removing the last bad subscription left the remaining
//     ones in a momentary RST window and we returned 500.
//   - Empty `all` outbounds is a valid steady state: the mihomo renderer's
//     empty-pool fallback emits a DIRECT-only config that loads cleanly.
//     Operators recovering from "all subs broken" want the data plane to
//     stay alive, not refuse to render.
//
// Hard errors (returned to caller) are reserved for problems caused by THIS
// refresh that the caller can act on: render failure, mihomo reload failure.
func RunRefresh(ctx context.Context, mgr *subscribe.Manager, r Renderer, c *dataplane.Controller) error {
	results, err := mgr.Refresh(ctx)
	if err != nil {
		slog.Warn("subscribe refresh had errors", "err", err)
	}
	all := mgr.AllOutbounds()
	if len(results) == 0 {
		slog.Warn("subscribe: no subscription produced nodes — rendering DIRECT-only fallback",
			"results", len(results), "outbounds", len(all))
	}
	if _, err := r.Write(all); err != nil {
		return fmt.Errorf("render: %w", err)
	}
	slog.Info("subscribe refreshed", "subscriptions", len(results), "nodes", len(all))
	if err := c.Reload(ctx, r.Path()); err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
