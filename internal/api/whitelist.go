package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

// whitelistDTO is the on-the-wire shape of GET /api/whitelist and PUT body.
//
// Domain side:
//   - Geosites: rule-set tags (must start with "geosite-", must exist on disk).
//   - DomainSuffix: bare-domain literals.
//
// IP side (added so apps that skip DNS still get whitelisted — see Telegram):
//   - Geoips: rule-set tags (must start with "geoip-", must exist on disk).
//   - IPCIDR: CIDR or bare IP literals; bare IPs normalized to /32 or /128.
//
// DNS side:
//   - FakeIPSkip: domains that must not receive a fakeip from the engine's
//     DNS server. Required for internal services whose hostnames resolve to
//     CN/LAN IPs but aren't on geosite-cn (paigod.work, feilian.cn, …).
//     Accepted formats: "+.example.com" / ".example.com" / "example.com"
//     — all normalised to "+." prefix. Synced to
//     cfg.DataPlane.DNS.FakeIPSkipSuffixes on every successful PUT.
type whitelistDTO struct {
	Mode         string   `json:"mode"`
	Geosites     []string `json:"geosites"`
	Geoips       []string `json:"geoips"`
	DomainSuffix []string `json:"domain_suffix"`
	IPCIDR       []string `json:"ip_cidr"`
	FakeIPSkip   []string `json:"fake_ip_skip"`
}

func (s *Server) handleWhitelistGet(w http.ResponseWriter, r *http.Request) {
	wl := s.deps.Cfg.DataPlane.Route.Whitelist
	writeJSON(w, http.StatusOK, whitelistDTO{
		Mode:         s.deps.Cfg.DataPlane.Route.Mode,
		Geosites:     append([]string{}, wl.Geosites...),
		Geoips:       append([]string{}, wl.Geoips...),
		DomainSuffix: append([]string{}, wl.DomainSuffix...),
		IPCIDR:       append([]string{}, wl.IPCIDR...),
		FakeIPSkip:   append([]string{}, wl.FakeIPSkip...),
	})
}

// handleWhitelistPut replaces the whole whitelist atomically. The PUT body's
// `mode` is informational — the actual mode comes from current cfg unless
// explicitly set to "overseas" (which clears the WL match rules at render
// time). Every geosite/geoip tag must exist in the embedded catalog; any
// referenced .srs file that isn't yet on disk is fetched on-demand from
// upstream (MetaCubeX/meta-rules-dat) before the cfg is mutated.
func (s *Server) handleWhitelistPut(w http.ResponseWriter, r *http.Request) {
	var dto whitelistDTO
	if err := json.NewDecoder(r.Body).Decode(&dto); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Reject empty/no-op bodies. An empty `{}` (or any body where mode !=
	// "overseas" and every match list is empty) is almost certainly a
	// caller bug — accepting it silently drops the whole whitelist and
	// breaks cross-border routing the moment the next render fires.
	// Caught in production 2026-06-09 when an automated test sent `{}`.
	// "overseas" mode legitimately has no match lists (everything routes
	// via the us-pool selector), so that's the only path through this
	// guard for an otherwise-empty body.
	if strings.ToLower(strings.TrimSpace(dto.Mode)) != "overseas" {
		empty := len(dto.Geosites) == 0 &&
			len(dto.Geoips) == 0 &&
			len(dto.DomainSuffix) == 0 &&
			len(dto.IPCIDR) == 0
		if empty {
			http.Error(w,
				"whitelist body has no match entries — set mode=\"overseas\" to intentionally clear, or include at least one of geosites/geoips/domain_suffix/ip_cidr",
				http.StatusBadRequest)
			return
		}
	}

	// Validation pass — catalog presence only, no network. Never mutate
	// cfg if validation fails.
	geosites, err := s.canonicalizeRuleSetTags(dto.Geosites, "geosite-")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	geoips, err := s.canonicalizeRuleSetTags(dto.Geoips, "geoip-")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
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

	cidrs, err := normalizeCIDRs(dto.IPCIDR)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Normalise fake_ip_skip entries to "+.<suffix>" form (mihomo/sing-box
	// require this prefix; we accept bare / leading-dot forms for convenience).
	fakeSkip := make([]string, 0, len(dto.FakeIPSkip))
	seenFS := map[string]bool{}
	for _, raw := range dto.FakeIPSkip {
		s := strings.TrimSpace(strings.ToLower(raw))
		if s == "" {
			continue
		}
		if s[0] == '.' {
			s = "+" + s
		} else if len(s) < 2 || s[:2] != "+." {
			s = "+." + s
		}
		if seenFS[s] {
			continue
		}
		seenFS[s] = true
		fakeSkip = append(fakeSkip, s)
	}

	mode := strings.ToLower(strings.TrimSpace(dto.Mode))
	if mode == "" {
		mode = s.deps.Cfg.DataPlane.Route.Mode
	}
	if mode != "overseas" && mode != "whitelist" {
		http.Error(w, fmt.Sprintf("invalid mode %q (want overseas|whitelist)", mode), http.StatusBadRequest)
		return
	}

	// Fetch any selected rule-sets that aren't on disk yet. EnsureInstalled
	// is a no-op if <name>.srs is already present, so re-PUTs of the same
	// list don't re-download. Each tag has its own per-name lock inside the
	// manager; concurrent PUTs of the same tag coalesce to one HTTP fetch.
	fetchCtx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	for _, name := range append(append([]string{}, geosites...), geoips...) {
		if err := s.deps.RuleSets.EnsureInstalled(fetchCtx, name); err != nil {
			http.Error(w, fmt.Sprintf("download %s: %s", name, err), http.StatusBadGateway)
			return
		}
	}

	// Mutate + persist + reload.
	err = s.deps.Store.Mutate(s.deps.Cfg, func(c *config.Config) error {
		c.DataPlane.Route.Mode = mode
		c.DataPlane.Route.Whitelist.Geosites = geosites
		c.DataPlane.Route.Whitelist.Geoips = geoips
		c.DataPlane.Route.Whitelist.DomainSuffix = suffixes
		c.DataPlane.Route.Whitelist.IPCIDR = cidrs
		c.DataPlane.Route.Whitelist.FakeIPSkip = fakeSkip
		// Sync fake_ip_skip to the DNS config layer so the renderer
		// picks it up without a separate config update.
		c.DataPlane.DNS.FakeIPSkipSuffixes = fakeSkip
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

	// Async refresh of the resolved snapshot (domains + ip_cidrs). Don't
	// block the PUT response on it — the upstream fetch can take several
	// seconds, and the caller already has confirmation that the WL itself
	// was applied. Stale /api/whitelist/resolved snapshots are explicitly
	// marked stale=true.
	if s.deps.Expander != nil {
		go s.refreshExpander()
	}

	s.handleWhitelistGet(w, r)
}

// canonicalizeRuleSetTags trims, dedups, validates the prefix, and confirms
// each tag exists in the embedded catalog. Returns the canonical list (in
// input order, deduped). The actual on-disk presence is enforced separately
// by the EnsureInstalled call in the PUT flow.
func (s *Server) canonicalizeRuleSetTags(in []string, prefix string) (config.StringList, error) {
	out := make(config.StringList, 0, len(in))
	seen := map[string]bool{}
	for _, name := range in {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !strings.HasPrefix(name, prefix) {
			return nil, fmt.Errorf("rule-set %q does not start with %q", name, prefix)
		}
		if _, ok := s.deps.RuleSets.Lookup(name); !ok {
			return nil, fmt.Errorf("unknown rule-set %q (not in catalog)", name)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}

// normalizeCIDRs validates ip_cidr entries and converts bare IPs to /32 (v4)
// or /128 (v6). Returns canonical form (lowercase IPv6, leading zero stripped)
// so two equivalent inputs dedupe correctly.
func normalizeCIDRs(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		var canonical string
		if strings.Contains(raw, "/") {
			p, err := netip.ParsePrefix(raw)
			if err != nil {
				return nil, fmt.Errorf("invalid ip_cidr %q: %s", raw, err)
			}
			canonical = p.Masked().String()
		} else {
			a, err := netip.ParseAddr(raw)
			if err != nil {
				return nil, fmt.Errorf("invalid ip_cidr %q: %s", raw, err)
			}
			bits := 32
			if a.Is6() {
				bits = 128
			}
			canonical = netip.PrefixFrom(a, bits).String()
		}
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		out = append(out, canonical)
	}
	return out, nil
}

// refreshExpander runs the v2fly expansion against the current cfg in a
// fresh context so it survives the request that triggered it.
func (s *Server) refreshExpander() {
	if s.deps.Expander == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	wl := s.deps.Cfg.DataPlane.Route.Whitelist
	if _, err := s.deps.Expander.Refresh(ctx, wl.Geosites, wl.Geoips, wl.DomainSuffix, wl.IPCIDR); err != nil {
		// Already logged inside Refresh; nothing else to do here — old
		// snapshot is preserved and marked stale.
		_ = err
	}
}

// handleWhitelistResolved returns the resolved snapshot — geosites/geoips
// expanded into flat domain + IP CIDR lists, merged with the literal
// domain_suffix / ip_cidr entries. Used by FeiLian's "极速模式" which only
// accepts concrete domain + CIDR values (not rule-set tags).
func (s *Server) handleWhitelistResolved(w http.ResponseWriter, r *http.Request) {
	if s.deps.Expander == nil {
		http.Error(w, "expander not configured", http.StatusServiceUnavailable)
		return
	}
	snap := s.deps.Expander.Snapshot()
	if snap == nil {
		// First-boot: no cache yet. Trigger one inline (best-effort) so the
		// caller doesn't have to poll. Still bound by request timeout.
		wl := s.deps.Cfg.DataPlane.Route.Whitelist
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		built, err := s.deps.Expander.Refresh(ctx, wl.Geosites, wl.Geoips, wl.DomainSuffix, wl.IPCIDR)
		if err != nil || built == nil {
			http.Error(w, "expansion not yet available — try again in a few seconds", http.StatusServiceUnavailable)
			return
		}
		snap = built
	}
	writeJSON(w, http.StatusOK, snap)
}

// rerenderAndReload re-renders the data-plane config from the cached subscription
// state and triggers a data-plane reload. Used by mutating API endpoints
// (whitelist PUT, refresh-interval PUT) where the cfg has changed without
// touching subscriptions. Subscription writes use RunRefresh instead.
func (s *Server) rerenderAndReload(ctx context.Context) error {
	// Renderer holds cfg.DataPlane by value — must reseed engine-relevant
	// fields after a Mutate before Write reads stale state.
	wl := s.deps.Cfg.DataPlane.Route.Whitelist
	s.deps.Renderer.SetWhitelist(s.deps.Cfg.DataPlane.Route.Mode, wl)
	// Sync fake_ip_skip to the renderer's DNS config layer so the updated
	// fake-ip-filter is emitted without requiring a full config reload.
	s.deps.Renderer.SetFakeIPSkip(s.deps.Cfg.DataPlane.DNS.FakeIPSkipSuffixes)
	out := s.deps.Subscribe.AllOutbounds()
	if _, err := s.deps.Renderer.Write(out); err != nil {
		return fmt.Errorf("render: %w", err)
	}
	return s.deps.Controller.Reload(ctx, s.deps.Renderer.Path())
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
