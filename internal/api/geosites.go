package api

import (
	"net/http"
	"os"
	"sort"
	"strings"
)

type geositesDTO struct {
	Available []string `json:"available"`
	Active    []string `json:"active"`
}

// handleGeositesGet returns the geosite catalog. `available` = .srs files in
// the rule-sets dir minus the infra entries (geosite-cn / geoip-cn) which are
// not user-toggleable. `active` = whichever subset gateway.yaml's whitelist
// currently lists. The intersection (active ∩ available) is what the renderer
// actually wires; entries in `active` but not `available` indicate a stale
// gateway.yaml referencing a missing .srs (will warn at render time but won't
// crash sing-box because we emit type=local).
func (s *Server) handleGeositesGet(w http.ResponseWriter, r *http.Request) {
	available, err := scanRuleSetsDir(s.deps.Cfg.SingBox.RuleSetsDir)
	if err != nil {
		http.Error(w, "cannot scan rule-sets dir: "+err.Error(), http.StatusInternalServerError)
		return
	}
	active := make([]string, 0, len(s.deps.Cfg.SingBox.Route.Whitelist.Geosites))
	for _, g := range s.deps.Cfg.SingBox.Route.Whitelist.Geosites {
		active = append(active, g.Name)
	}
	writeJSON(w, http.StatusOK, geositesDTO{
		Available: available,
		Active:    active,
	})
}

// scanRuleSetsDir lists "<tag>" for every "<tag>.srs" in dir, excluding
// "geosite-cn" and "geoip-cn" (those are infrastructure, not WL toggles).
// Returns sorted names. Empty result if dir doesn't exist (during install).
func scanRuleSetsDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".srs") {
			continue
		}
		tag := strings.TrimSuffix(name, ".srs")
		if tag == "geosite-cn" || tag == "geoip-cn" {
			continue
		}
		out = append(out, tag)
	}
	sort.Strings(out)
	return out, nil
}
