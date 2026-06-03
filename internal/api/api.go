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
	"github.com/leap-gateway/leap-gateway/internal/nodeinfo"
	"github.com/leap-gateway/leap-gateway/internal/rulesets"
	"github.com/leap-gateway/leap-gateway/internal/singbox"
	"github.com/leap-gateway/leap-gateway/internal/subscribe"
	"github.com/leap-gateway/leap-gateway/internal/watchdog"
	"github.com/leap-gateway/leap-gateway/internal/whitelistexpand"
)

type Deps struct {
	Subscribe  *subscribe.Manager
	Scheduler  *subscribe.Scheduler
	Renderer   *singbox.Renderer
	Controller *singbox.Controller
	Cfg        *config.Config
	Store      *configstore.Store
	NodeInfo   *nodeinfo.Reporter
	Watchdog   *watchdog.Watchdog
	Expander   *whitelistexpand.Expander
	RuleSets   *rulesets.Manager
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

	mux.HandleFunc("GET /api/geosites", s.auth(s.handleGeositesGet))
	mux.HandleFunc("GET /api/rule-sets", s.auth(s.handleRuleSetsGet))

	mux.HandleFunc("GET /api/subscriptions", s.auth(s.handleSubscriptionsGet))
	mux.HandleFunc("POST /api/subscriptions", s.auth(s.handleSubscriptionsPost))
	mux.HandleFunc("PUT /api/subscriptions/{name}", s.auth(s.handleSubscriptionsPut))
	mux.HandleFunc("DELETE /api/subscriptions/{name}", s.auth(s.handleSubscriptionsDelete))

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
	singboxOK := s.deps.Controller.Health(r.Context()) == nil
	writeJSON(w, http.StatusOK, map[string]any{
		"last_refresh":  last,
		"node_count":    count,
		"subscriptions": len(results),
		"singbox_ok":    singboxOK,
	})
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
func RunRefresh(ctx context.Context, mgr *subscribe.Manager, r *singbox.Renderer, c *singbox.Controller) error {
	results, err := mgr.Refresh(ctx)
	if err != nil {
		slog.Warn("subscribe refresh had errors", "err", err)
	}
	if len(results) == 0 {
		return fmt.Errorf("no subscription produced any nodes")
	}
	all := mgr.AllOutbounds()
	if _, err := r.Write(all); err != nil {
		return fmt.Errorf("render: %w", err)
	}
	slog.Info("subscribe refreshed", "subscriptions", len(results), "nodes", len(all))
	if err := c.Reload(ctx); err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
