package api

import (
	"net/http"

	"github.com/leap-gateway/leap-gateway/internal/rulesets"
)

// ruleSetEntryDTO is one item of GET /api/rule-sets — name + url + category
// from the embedded catalog, plus runtime flags (installed: .srs is on disk;
// selected: tag is in cfg.DataPlane.Route.Whitelist).
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

// handleRuleSetsGet returns the full embedded catalog for both geosites and
// geoips, with per-item installed/selected flags computed against the
// rule-sets directory and the current whitelist. Also surfaces example values
// for the literal domain_suffix / ip_cidr fields so a UI can prefill them.
func (s *Server) handleRuleSetsGet(w http.ResponseWriter, _ *http.Request) {
	cat := s.deps.RuleSets.Catalog()
	wl := s.deps.Cfg.DataPlane.Route.Whitelist

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
