package api

import (
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/leap-gateway/leap-gateway/internal/rulesets"
)

// ruleSetEntryDTO is one item of GET /api/rule-sets — name + url + category
// from the embedded catalog, plus runtime flags (installed: .srs is on disk;
// selected: tag is in cfg.SingBox.Route.Whitelist).
type ruleSetEntryDTO struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Category  string `json:"category"`
	Installed bool   `json:"installed"`
	Selected  bool   `json:"selected"`
}

type ruleSetsSideDTO struct {
	Catalog              []ruleSetEntryDTO `json:"catalog"`
	DomainSuffixExamples []string          `json:"domain_suffix_examples,omitempty"`
	IPCIDRExamples       []string          `json:"ip_cidr_examples,omitempty"`
}

type ruleSetsDTO struct {
	Geosites ruleSetsSideDTO `json:"geosites"`
	Geoips   ruleSetsSideDTO `json:"geoips"`
}

// geositesDTO is the legacy shape of GET /api/geosites — the geosites half of
// the catalog projected into {available, active} where available = tags
// currently on disk. Retained for callers that haven't moved to /api/rule-sets.
type geositesDTO struct {
	Available []string `json:"available"`
	Active    []string `json:"active"`
}

// handleRuleSetsGet returns the full embedded catalog for both geosites and
// geoips, with per-item installed/selected flags computed against the
// rule-sets directory and the current whitelist. Also surfaces example values
// for the literal domain_suffix / ip_cidr fields so a UI can prefill them.
func (s *Server) handleRuleSetsGet(w http.ResponseWriter, _ *http.Request) {
	cat := s.deps.RuleSets.Catalog()
	wl := s.deps.Cfg.SingBox.Route.Whitelist

	dto := ruleSetsDTO{}
	dto.Geosites = ruleSetsSideDTO{
		Catalog:              projectCatalog(cat.Geosites, wl.Geosites, s.deps.RuleSets),
		DomainSuffixExamples: append([]string{}, cat.DomainSuffixExamples...),
	}
	dto.Geoips = ruleSetsSideDTO{
		Catalog:        projectCatalog(cat.Geoips, wl.Geoips, s.deps.RuleSets),
		IPCIDRExamples: append([]string{}, cat.IPCIDRExamples...),
	}
	writeJSON(w, http.StatusOK, dto)
}

// handleGeositesGet is a back-compat alias that exposes only geosites in
// the legacy {available, active} shape. Newer callers should use
// /api/rule-sets directly.
func (s *Server) handleGeositesGet(w http.ResponseWriter, _ *http.Request) {
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

// projectCatalog turns a catalog slice into the wire DTO, computing
// installed/selected for each entry.
func projectCatalog(items []rulesets.Item, selected []string, mgr *rulesets.Manager) []ruleSetEntryDTO {
	sel := make(map[string]bool, len(selected))
	for _, t := range selected {
		sel[t] = true
	}
	out := make([]ruleSetEntryDTO, 0, len(items))
	for _, it := range items {
		out = append(out, ruleSetEntryDTO{
			Name:      it.Name,
			URL:       it.URL,
			Category:  it.Category,
			Installed: mgr.IsInstalled(it.Name),
			Selected:  sel[it.Name],
		})
	}
	return out
}

// RuleSets is the partition of scanRuleSetsDir's output by upstream catalog.
type RuleSets struct {
	Geosites []string
	Geoips   []string
}

// scanRuleSetsDir lists "<tag>" for every "<tag>.srs" present in dir, split
// by prefix and excluding "geosite-cn" / "geoip-cn" (those are infrastructure
// for route classification, not user-toggleable). Empty result if dir does
// not exist (during install). Used by the legacy /api/geosites response.
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
