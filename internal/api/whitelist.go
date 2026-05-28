package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

// whitelistDTO is the on-the-wire shape of GET /api/whitelist and PUT body.
type whitelistDTO struct {
	Mode         string   `json:"mode"`
	Geosites     []string `json:"geosites"`
	DomainSuffix []string `json:"domain_suffix"`
}

func (s *Server) handleWhitelistGet(w http.ResponseWriter, r *http.Request) {
	wl := s.deps.Cfg.SingBox.Route.Whitelist
	geosites := make([]string, 0, len(wl.Geosites))
	for _, g := range wl.Geosites {
		geosites = append(geosites, g.Name)
	}
	writeJSON(w, http.StatusOK, whitelistDTO{
		Mode:         s.deps.Cfg.SingBox.Route.Mode,
		Geosites:     geosites,
		DomainSuffix: append([]string{}, wl.DomainSuffix...),
	})
}

// handleWhitelistPut replaces the whole whitelist atomically. The PUT body's
// `mode` is informational — the actual mode comes from current cfg unless
// explicitly set to "overseas" (which clears the WL match rules at render
// time). Geosites must all exist locally; otherwise the request fails before
// any state changes.
func (s *Server) handleWhitelistPut(w http.ResponseWriter, r *http.Request) {
	var dto whitelistDTO
	if err := json.NewDecoder(r.Body).Decode(&dto); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}

	available, err := scanRuleSetsDir(s.deps.Cfg.SingBox.RuleSetsDir)
	if err != nil {
		http.Error(w, "cannot scan rule-sets dir: "+err.Error(), http.StatusInternalServerError)
		return
	}
	availSet := make(map[string]bool, len(available))
	for _, name := range available {
		availSet[name] = true
	}

	// Validation pass — never mutate cfg if validation fails.
	geosites := make([]config.GeositeRef, 0, len(dto.Geosites))
	seenG := map[string]bool{}
	for _, name := range dto.Geosites {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !availSet[name] {
			http.Error(w, fmt.Sprintf("unknown geosite %q (not in %s)", name, s.deps.Cfg.SingBox.RuleSetsDir), http.StatusBadRequest)
			return
		}
		if seenG[name] {
			continue
		}
		seenG[name] = true
		geosites = append(geosites, config.GeositeRef{
			Name: name,
			URL:  guessGeositeURL(name),
		})
	}

	suffixes := make([]string, 0, len(dto.DomainSuffix))
	seenS := map[string]bool{}
	for _, sx := range dto.DomainSuffix {
		sx = strings.TrimSpace(strings.ToLower(sx))
		if sx == "" {
			continue
		}
		if err := validateDomainSuffix(sx); err != nil {
			http.Error(w, fmt.Sprintf("invalid domain_suffix %q: %s", sx, err), http.StatusBadRequest)
			return
		}
		if seenS[sx] {
			continue
		}
		seenS[sx] = true
		suffixes = append(suffixes, sx)
	}

	mode := strings.ToLower(strings.TrimSpace(dto.Mode))
	if mode == "" {
		mode = s.deps.Cfg.SingBox.Route.Mode
	}
	if mode != "overseas" && mode != "whitelist" {
		http.Error(w, fmt.Sprintf("invalid mode %q (want overseas|whitelist)", mode), http.StatusBadRequest)
		return
	}

	// Mutate + persist + reload.
	err = s.deps.Store.Mutate(s.deps.Cfg, func(c *config.Config) error {
		c.SingBox.Route.Mode = mode
		c.SingBox.Route.Whitelist.Geosites = geosites
		c.SingBox.Route.Whitelist.DomainSuffix = suffixes
		return nil
	})
	if err != nil {
		http.Error(w, "persist: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := s.rerenderAndReload(r.Context()); err != nil {
		http.Error(w, "reload: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.handleWhitelistGet(w, r)
}

// rerenderAndReload re-renders the sing-box config from the cached subscription
// outbounds and restarts leap-singbox. Used by WL writes (no need to re-fetch
// subscriptions). Subscription writes use RunRefresh instead.
func (s *Server) rerenderAndReload(ctx context.Context) error {
	out := s.deps.Subscribe.AllOutbounds()
	if _, err := s.deps.Renderer.Write(out); err != nil {
		return fmt.Errorf("render: %w", err)
	}
	return s.deps.Controller.Reload(ctx)
}

func validateDomainSuffix(s string) error {
	if strings.ContainsAny(s, " \t\n\r") {
		return fmt.Errorf("contains whitespace")
	}
	if strings.Contains(s, "://") {
		return fmt.Errorf("looks like a URL, want bare domain")
	}
	if strings.Contains(s, "/") {
		return fmt.Errorf("contains '/'")
	}
	if !strings.Contains(s, ".") {
		return fmt.Errorf("not a domain (missing '.')")
	}
	if strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
		return fmt.Errorf("leading/trailing dot")
	}
	return nil
}

// guessGeositeURL infers the official sagernet URL for a geosite tag. Used
// when the API operator gave us only a name — the URL is still recorded in
// gateway.yaml for traceability, even though the renderer reads .srs files
// from disk and never fetches.
func guessGeositeURL(name string) string {
	return "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/" + name + ".srs"
}
