package api

import (
	"net/http"
	"os"
	"sort"
	"strings"
)

// ruleSetsDTO is the shape of GET /api/rule-sets — separates the two upstream
// catalogs (sing-geosite vs sing-geoip) so callers can build proper UIs without
// guessing by tag prefix.
type ruleSetsDTO struct {
	Geosites struct {
		Available []string `json:"available"`
		Active    []string `json:"active"`
	} `json:"geosites"`
	Geoips struct {
		Available []string `json:"available"`
		Active    []string `json:"active"`
	} `json:"geoips"`
}

// geositesDTO is the legacy shape of GET /api/geosites. Retained as an alias
// over rule-sets.geosites for callers that haven't moved to /api/rule-sets.
type geositesDTO struct {
	Available []string `json:"available"`
	Active    []string `json:"active"`
}

// handleRuleSetsGet returns BOTH geosite and geoip catalogs. The `available`
// arrays come from .srs files actually present in RuleSetsDir (minus the
// infra entries geosite-cn / geoip-cn which are wired by the renderer for
// route, not user-toggleable). The `active` arrays come from the
// whitelist.geosites / whitelist.geoips fields in gateway.yaml.
func (s *Server) handleRuleSetsGet(w http.ResponseWriter, r *http.Request) {
	avail, err := scanRuleSetsDir(s.deps.Cfg.SingBox.RuleSetsDir)
	if err != nil {
		http.Error(w, "cannot scan rule-sets dir: "+err.Error(), http.StatusInternalServerError)
		return
	}
	wl := s.deps.Cfg.SingBox.Route.Whitelist
	dto := ruleSetsDTO{}
	dto.Geosites.Available = avail.Geosites
	dto.Geosites.Active = append([]string{}, wl.Geosites...)
	dto.Geoips.Available = avail.Geoips
	dto.Geoips.Active = append([]string{}, wl.Geoips...)
	writeJSON(w, http.StatusOK, dto)
}

// handleGeositesGet is a back-compat alias for /api/rule-sets that exposes
// only the geosites half. Newer callers should use /api/rule-sets directly.
func (s *Server) handleGeositesGet(w http.ResponseWriter, r *http.Request) {
	avail, err := scanRuleSetsDir(s.deps.Cfg.SingBox.RuleSetsDir)
	if err != nil {
		http.Error(w, "cannot scan rule-sets dir: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, geositesDTO{
		Available: avail.Geosites,
		Active:    append([]string{}, s.deps.Cfg.SingBox.Route.Whitelist.Geosites...),
	})
}

// RuleSets categorizes scanRuleSetsDir's output by upstream catalog.
type RuleSets struct {
	Geosites []string // tags starting with "geosite-" (excluding geosite-cn)
	Geoips   []string // tags starting with "geoip-"   (excluding geoip-cn)
}

// scanRuleSetsDir lists "<tag>" for every "<tag>.srs" in dir, partitioned
// into Geosites and Geoips by prefix and excluding "geosite-cn" / "geoip-cn"
// (those are infrastructure, not WL toggles). Returns sorted names. Empty
// result if dir doesn't exist (during install).
func scanRuleSetsDir(dir string) (RuleSets, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return RuleSets{}, nil
		}
		return RuleSets{}, err
	}
	var rs RuleSets
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".srs") {
			continue
		}
		tag := strings.TrimSuffix(name, ".srs")
		switch {
		case tag == "geosite-cn" || tag == "geoip-cn":
			// infra; not user-toggleable
		case strings.HasPrefix(tag, "geosite-"):
			rs.Geosites = append(rs.Geosites, tag)
		case strings.HasPrefix(tag, "geoip-"):
			rs.Geoips = append(rs.Geoips, tag)
		}
	}
	sort.Strings(rs.Geosites)
	sort.Strings(rs.Geoips)
	return rs, nil
}
