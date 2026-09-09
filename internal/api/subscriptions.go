package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

type subscriptionDTO struct {
	Name        string `json:"name"`
	URL         string `json:"url"` // masked in GET responses
	Enabled     bool   `json:"enabled"`
	Format      string `json:"format,omitempty"`
	UserAgent   string `json:"user_agent,omitempty"`
	NodesCount  int    `json:"nodes_count,omitempty"`
	LastRefresh string `json:"last_refresh,omitempty"`
}

func (s *Server) handleSubscriptionsGet(w http.ResponseWriter, _ *http.Request) {
	results, last := s.deps.Subscribe.Snapshot()
	nodesByName := make(map[string]int, len(results))
	for _, r := range results {
		nodesByName[r.Name] = len(r.Outbounds)
	}
	out := make([]subscriptionDTO, 0, len(s.deps.Cfg.Subscriptions))
	for _, e := range s.deps.Cfg.Subscriptions {
		dto := subscriptionDTO{
			Name:       e.Name,
			URL:        maskURLToken(e.URL),
			Enabled:    e.Enabled,
			Format:     e.Format,
			UserAgent:  e.UserAgent,
			NodesCount: nodesByName[e.Name],
		}
		if !last.IsZero() {
			dto.LastRefresh = last.UTC().Format("2006-01-02T15:04:05Z")
		}
		out = append(out, dto)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSubscriptionsPost(w http.ResponseWriter, r *http.Request) {
	var dto subscriptionDTO
	if err := json.NewDecoder(r.Body).Decode(&dto); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	dto.Name = strings.TrimSpace(dto.Name)
	dto.URL = strings.TrimSpace(dto.URL)
	if dto.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if err := validateSubscriptionURL(dto.URL); err != nil {
		http.Error(w, "invalid url: "+err.Error(), http.StatusBadRequest)
		return
	}
	for _, e := range s.deps.Cfg.Subscriptions {
		if e.Name == dto.Name {
			http.Error(w, fmt.Sprintf("subscription %q already exists", dto.Name), http.StatusConflict)
			return
		}
	}

	new := config.SubscriptionEntry{
		Name:      dto.Name,
		URL:       dto.URL,
		Format:    defaultIfEmpty(dto.Format, "auto"),
		Enabled:   true, // default-on for newly added subscriptions
		UserAgent: strings.TrimSpace(dto.UserAgent),
	}
	err := s.deps.Store.Mutate(s.deps.Cfg, func(c *config.Config) error {
		c.Subscriptions = append(c.Subscriptions, new)
		return nil
	})
	if err != nil {
		http.Error(w, "persist: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// New URL → must re-fetch + re-render + reload.
	if err := s.refreshAfterEdit(r); err != nil {
		http.Error(w, "refresh: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	s.handleSubscriptionsGet(w, r)
}

func (s *Server) handleSubscriptionsPut(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	var dto subscriptionDTO
	if err := json.NewDecoder(r.Body).Decode(&dto); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	dto.URL = strings.TrimSpace(dto.URL)
	if err := validateSubscriptionURL(dto.URL); err != nil {
		http.Error(w, "invalid url: "+err.Error(), http.StatusBadRequest)
		return
	}

	idx := indexOfSubscription(s.deps.Cfg.Subscriptions, name)
	if idx < 0 {
		http.Error(w, fmt.Sprintf("subscription %q not found", name), http.StatusNotFound)
		return
	}

	newUA := strings.TrimSpace(dto.UserAgent)
	err := s.deps.Store.Mutate(s.deps.Cfg, func(c *config.Config) error {
		c.Subscriptions[idx].URL = dto.URL
		c.Subscriptions[idx].UserAgent = newUA
		return nil
	})
	if err != nil {
		http.Error(w, "persist: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.refreshAfterEdit(r); err != nil {
		http.Error(w, "refresh: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.handleSubscriptionsGet(w, r)
}

func (s *Server) handleSubscriptionsDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	idx := indexOfSubscription(s.deps.Cfg.Subscriptions, name)
	if idx < 0 {
		http.Error(w, fmt.Sprintf("subscription %q not found", name), http.StatusNotFound)
		return
	}
	err := s.deps.Store.Mutate(s.deps.Cfg, func(c *config.Config) error {
		c.Subscriptions = append(c.Subscriptions[:idx], c.Subscriptions[idx+1:]...)
		return nil
	})
	if err != nil {
		http.Error(w, "persist: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.refreshAfterEdit(r); err != nil {
		http.Error(w, "refresh: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// refreshAfterEdit triggers the full subscribe→render→reload chain. Used by
// every subscription write because the cached outbound list is now stale —
// new URL means new nodes, removed URL means dropped nodes. Also re-seeds
// the renderer's subscription order so urltest-primary/-backup splits track
// edits to the yaml.
//
// We have to call SetEntries first because Manager.entries is a defensive
// copy made at construction; without re-seeding, Refresh would iterate the
// stale set. Same reason for Renderer.SetSubscriptions.
func (s *Server) refreshAfterEdit(r *http.Request) error {
	s.deps.Subscribe.SetEntries(s.deps.Cfg.Subscriptions)
	s.deps.Renderer.SetSubscriptions(s.deps.Cfg.Subscriptions)
	return RunRefresh(r.Context(), s.deps.Subscribe, s.deps.Renderer, s.deps.Controller, s.deps.Notifier)
}

func validateSubscriptionURL(s string) error {
	if s == "" {
		return fmt.Errorf("empty")
	}
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("missing host")
	}
	return nil
}

// maskURLToken hides obvious secret-looking query values (token=, key=,
// password=) by substring replacement so the visible asterisks survive in
// the displayed URL (url.Values.Encode would percent-escape them). Other
// query values pass through verbatim.
func maskURLToken(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return raw
	}
	pairs := strings.Split(u.RawQuery, "&")
	changed := false
	for i, kv := range pairs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" {
			continue
		}
		switch strings.ToLower(k) {
		case "token", "key", "password", "auth", "secret":
			pairs[i] = k + "=" + maskMiddle(v)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	u.RawQuery = strings.Join(pairs, "&")
	return u.String()
}

func maskMiddle(s string) string {
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "***" + s[len(s)-4:]
}

func indexOfSubscription(list []config.SubscriptionEntry, name string) int {
	for i, e := range list {
		if e.Name == name {
			return i
		}
	}
	return -1
}

func defaultIfEmpty(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
