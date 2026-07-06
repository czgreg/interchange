// Package whitelistexpand resolves the whitelist (geosite/geoip rule-set tags
// + raw domain_suffix / ip_cidr) into flat lists of concrete domain suffixes
// and IP CIDRs — what /api/whitelist/resolved serves to FeiLian's "极速模式"
// (which only accepts literal domain + CIDR lists, not rule-set tags).
//
// Sources:
//   - Domains: v2fly/domain-list-community via jsdelivr CDN, github raw
//     fallback. The PoC node was verified to reach jsdelivr in <1s.
//   - IP CIDRs: local .srs blobs (already shipped in the deploy tarball)
//     decompiled via `sing-box rule-set decompile`. No network needed.
//
// Cache strategy:
//   - In-memory snapshot served by the API.
//   - Disk persistence so a restart doesn't immediately serve "no data"
//     while the upstream round-trip completes.
//   - Refresh is async. The API returns the last good snapshot with its
//     timestamp; callers can detect staleness via the .Stale field.
package whitelistexpand

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/leaphttp"
)

const (
	defaultCachePath = "/var/lib/leap/whitelist-resolved.json"
	// legacyCachePath is the pre-rename location. Loaded as a fallback so the
	// first start after upgrade doesn't serve empty until the warm refresh
	// finishes.
	legacyCachePath = "/var/lib/leap/whitelist-domains.json"
	httpTimeout     = 12 * time.Second
)

// Sources are tried in order. jsdelivr first because it's fast + un-blocked
// from CN; github raw is the fallback for rare CN-DNS-glitch moments.
var defaultSources = []string{
	"https://cdn.jsdelivr.net/gh/v2fly/domain-list-community@master/data/",
	"https://raw.githubusercontent.com/v2fly/domain-list-community/master/data/",
}

// Input echoes back what the operator wrote in the whitelist — handy for the
// API consumer to confirm the snapshot reflects current cfg.
type Input struct {
	Geosites     []string `json:"geosites"`
	Geoips       []string `json:"geoips"`
	DomainSuffix []string `json:"domain_suffix"`
	IPCIDR       []string `json:"ip_cidr"`
}

// Snapshot is what GET /api/whitelist/resolved returns.
type Snapshot struct {
	Input         Input     `json:"input"`
	Domains       []string  `json:"domains"`
	IPCIDRs       []string  `json:"ip_cidrs"`
	DomainsCount  int       `json:"domains_count"`
	IPCIDRsCount  int       `json:"ip_cidrs_count"`
	LastBuiltAt   time.Time `json:"last_built_at"`
	Source        string    `json:"source,omitempty"` // which CDN base served the geosite data
	Stale         bool      `json:"stale,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
}

// Expander owns the cache + the build worker.
type Expander struct {
	cachePath   string
	proxyURL    string // empty = direct dial; set via WithProxy. See package leaphttp for why this exists.
	client      *http.Client
	sources     []string
	ruleSetsDir string // "" disables geoip expansion (sing-box-mode lookup path)
	singboxBin  string // "" disables geoip expansion
	// srsCacheDir, when non-empty, switches geoip expansion to a private
	// cache: each geoip-* tag's .srs is fetched on demand from MetaCubeX
	// /sing/ branch and stored here. Required under engine=mihomo, where
	// the active rule-sets dir holds .mrs files that sing-box CLI can't
	// decompile. srsHTTPClient routes the fetch through the local proxy
	// engine's HTTP inbound (via leaphttp's proxy-or-direct fallback) so
	// the upstream connect egresses through the pool, not direct into the GFW.
	srsCacheDir   string
	srsHTTPClient *http.Client

	mu   sync.RWMutex
	snap *Snapshot

	buildMu sync.Mutex // serializes refreshes — multiple PUTs in a row should not stampede the upstream
}

func New(cachePath string) *Expander {
	if cachePath == "" {
		cachePath = defaultCachePath
	}
	return &Expander{
		cachePath: cachePath,
		// Default to direct-dial. Production callers MUST follow up with
		// WithProxy(LeapInternalProxyURL) — otherwise this client dials
		// overseas hosts directly through the host's resolver, which sees
		// mihomo's fake IPs and times out. See package leaphttp.
		client:  leaphttp.NewClient("", httpTimeout, "whitelistexpand-domain"),
		sources: defaultSources,
	}
}

// WithProxy wires the leap-internal HTTP proxy URL (typically
// http://127.0.0.1:11080 = singbox.LeapInternalProxyURL) for ALL outbound
// HTTP from this expander — the v2fly domain fetcher AND the on-demand
// .srs cache fetcher.
//
// Without this, the expander's `&http.Client{}` dials the overseas source
// directly into the GFW; result is indefinite timeouts on every category
// fetch (caught in production 2026-06-06 — domain count silently dropped
// from 2400+ to 6 = literals only).
//
// The returned client falls back to direct dial if the proxy is
// unreachable, so the bootstrap window (mihomo not yet up) and data-
// plane crash periods don't completely stall expansion.
//
// Idempotent: safe to call multiple times; takes effect from next fetch.
func (e *Expander) WithProxy(proxyURL string) *Expander {
	e.proxyURL = proxyURL
	e.client = leaphttp.NewClient(proxyURL, httpTimeout, "whitelistexpand-domain")
	if e.srsCacheDir != "" {
		e.srsHTTPClient = leaphttp.NewClient(proxyURL, httpTimeout, "whitelistexpand-srs")
	}
	return e
}

// WithRuleSets configures the local .srs directory and sing-box binary used
// for geoip expansion. Without these, geoip-* tags fall through (logged as a
// warning) but ip_cidr literals still appear in the output.
func (e *Expander) WithRuleSets(dir, singboxBin string) *Expander {
	e.ruleSetsDir = dir
	e.singboxBin = singboxBin
	return e
}

// WithMihomoSrsCache enables geoip expansion under engine=mihomo. The active
// proxy engine's rule-sets dir holds .mrs files (mihomo's binary format) that
// sing-box CLI can't decompile, so whitelistexpand maintains its own private
// .srs cache at cacheDir, fetching from MetaCubeX /sing/ branch on demand.
//
// Pair with WithProxy(...) to route the fetch through the local proxy
// engine's loopback inbound (egresses through the pool). When WithProxy
// hasn't been called the srs fetcher dials direct — which usually fails
// against the GFW; deliberate caller responsibility, not silently swallowed.
func (e *Expander) WithMihomoSrsCache(cacheDir string) *Expander {
	e.srsCacheDir = cacheDir
	e.srsHTTPClient = leaphttp.NewClient(e.proxyURL, httpTimeout, "whitelistexpand-srs")
	return e
}

// LoadFromDisk seeds the in-memory snapshot from the on-disk cache. Returns
// nil quietly when the file doesn't exist (first run after install). Tries
// the new cache path first, then falls back to the pre-rename location so
// upgrades don't lose state.
func (e *Expander) LoadFromDisk() error {
	candidates := []string{e.cachePath}
	if e.cachePath != legacyCachePath {
		candidates = append(candidates, legacyCachePath)
	}
	var data []byte
	var fromPath string
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err == nil {
			data = b
			fromPath = p
			break
		}
		if !os.IsNotExist(err) {
			return err
		}
	}
	if data == nil {
		return nil
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		// Old shape (pre-rename) is structurally different — best-effort to
		// extract what we can, then drop the file so the next refresh writes
		// the new shape from scratch.
		return e.loadLegacy(data, fromPath)
	}
	e.mu.Lock()
	e.snap = &s
	e.mu.Unlock()
	slog.Info("whitelistexpand: loaded cache from disk",
		"path", fromPath, "domains", s.DomainsCount, "ip_cidrs", s.IPCIDRsCount,
		"built_at", s.LastBuiltAt)
	return nil
}

// loadLegacy salvages a pre-rename snapshot ({geosites, geoips, domain_suffix,
// ip_cidr, domains, count, ...}) into the new shape. ip_cidrs is left empty —
// it'll fill in on first Refresh.
func (e *Expander) loadLegacy(data []byte, path string) error {
	var legacy struct {
		Geosites     []string  `json:"geosites"`
		Geoips       []string  `json:"geoips"`
		DomainSuffix []string  `json:"domain_suffix"`
		IPCIDR       []string  `json:"ip_cidr"`
		Domains      []string  `json:"domains"`
		LastBuiltAt  time.Time `json:"last_built_at"`
		Source       string    `json:"source"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return fmt.Errorf("decode %s (legacy): %w", path, err)
	}
	s := &Snapshot{
		Input: Input{
			Geosites:     legacy.Geosites,
			Geoips:       legacy.Geoips,
			DomainSuffix: legacy.DomainSuffix,
			IPCIDR:       legacy.IPCIDR,
		},
		Domains:      legacy.Domains,
		DomainsCount: len(legacy.Domains),
		LastBuiltAt:  legacy.LastBuiltAt,
		Source:       legacy.Source,
		Stale:        true,
		LastError:    "loaded legacy snapshot — ip_cidrs pending first refresh",
	}
	e.mu.Lock()
	e.snap = s
	e.mu.Unlock()
	slog.Info("whitelistexpand: migrated legacy cache from disk",
		"path", path, "domains", len(legacy.Domains))
	return nil
}

// Snapshot returns the latest known expansion. May be nil if no expansion has
// ever succeeded AND no on-disk cache existed.
func (e *Expander) Snapshot() *Snapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.snap == nil {
		return nil
	}
	cp := *e.snap
	cp.Domains = append([]string(nil), e.snap.Domains...)
	cp.IPCIDRs = append([]string(nil), e.snap.IPCIDRs...)
	cp.Input.Geosites = append([]string(nil), e.snap.Input.Geosites...)
	cp.Input.Geoips = append([]string(nil), e.snap.Input.Geoips...)
	cp.Input.DomainSuffix = append([]string(nil), e.snap.Input.DomainSuffix...)
	cp.Input.IPCIDR = append([]string(nil), e.snap.Input.IPCIDR...)
	return &cp
}

// Refresh runs the expansion against the supplied WL inputs and atomically
// replaces the snapshot. On failure it preserves the previous snapshot but
// marks it stale + records the error.
//
// Domains expand from geosites (v2fly walk) + domain_suffix (literal).
// IPCIDRs expand from geoips (sing-box rule-set decompile) + ip_cidr (literal).
// All four input lists are echoed in Snapshot.Input for consumer transparency.
func (e *Expander) Refresh(ctx context.Context, geosites, geoips []string, suffix, ipCIDR []string) (*Snapshot, error) {
	e.buildMu.Lock()
	defer e.buildMu.Unlock()

	domains, source, domainErr := e.expandDomains(ctx, geosites, suffix)
	ipCIDRs, ipErr := e.expandIPCIDRs(geoips, ipCIDR)

	if len(domains) == 0 && len(ipCIDRs) == 0 {
		// Don't overwrite a previous good snapshot with empty just because
		// the entire upstream is unreachable for a moment.
		msg := "no rules expanded — upstream unreachable?"
		if domainErr != nil {
			msg = domainErr.Error()
		} else if ipErr != nil {
			msg = ipErr.Error()
		}
		e.markStale(msg)
		return e.Snapshot(), fmt.Errorf("expansion produced 0 entries: %s", msg)
	}

	snap := &Snapshot{
		Input: Input{
			Geosites:     append([]string(nil), geosites...),
			Geoips:       append([]string(nil), geoips...),
			DomainSuffix: append([]string(nil), suffix...),
			IPCIDR:       append([]string(nil), ipCIDR...),
		},
		Domains:      domains,
		IPCIDRs:      ipCIDRs,
		DomainsCount: len(domains),
		IPCIDRsCount: len(ipCIDRs),
		LastBuiltAt:  time.Now().UTC(),
		Source:       source,
	}

	e.mu.Lock()
	e.snap = snap
	e.mu.Unlock()

	if err := e.saveToDisk(snap); err != nil {
		// Disk write failure isn't fatal — in-memory is fresh, next restart
		// will simply re-fetch.
		slog.Warn("whitelistexpand: cache write failed", "err", err)
	}
	slog.Info("whitelistexpand: refresh ok",
		"domains", snap.DomainsCount, "ip_cidrs", snap.IPCIDRsCount,
		"source", source)
	return e.Snapshot(), nil
}

func (e *Expander) expandDomains(ctx context.Context, geosites, suffix []string) ([]string, string, error) {
	tags := make([]string, 0, len(geosites))
	for _, g := range geosites {
		tags = append(tags, strings.TrimPrefix(g, "geosite-"))
	}
	domains := make(map[string]struct{})
	seen := make(map[string]struct{})
	var sourceUsed string
	for _, t := range tags {
		used, err := e.parseGeosite(ctx, t, seen, domains)
		if err != nil {
			slog.Warn("whitelistexpand: category failed", "tag", t, "err", err)
			continue
		}
		if sourceUsed == "" {
			sourceUsed = used
		}
	}
	for _, s := range suffix {
		domains[strings.ToLower(strings.TrimSpace(s))] = struct{}{}
	}
	delete(domains, "")
	out := make([]string, 0, len(domains))
	for d := range domains {
		out = append(out, d)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, sourceUsed, fmt.Errorf("no domains expanded")
	}
	return out, sourceUsed, nil
}

// expandIPCIDRs walks each geoip-* tag through `sing-box rule-set decompile`
// and merges with the literal ip_cidr entries. Pure-local op (no network) —
// .srs files were baked into the install tarball.
func (e *Expander) expandIPCIDRs(geoips, ipCIDR []string) ([]string, error) {
	cidrs := make(map[string]struct{})
	for _, raw := range ipCIDR {
		c := strings.TrimSpace(raw)
		if c != "" {
			cidrs[c] = struct{}{}
		}
	}
	if e.ruleSetsDir == "" || e.singboxBin == "" {
		if len(geoips) > 0 {
			slog.Warn("whitelistexpand: geoip expansion disabled (rule_sets_dir or singbox binary unset); ip_cidr literals only", "geoips", geoips)
		}
	} else {
		for _, tag := range geoips {
			expanded, err := e.decompileGeoip(tag)
			if err != nil {
				slog.Warn("whitelistexpand: geoip decompile failed", "tag", tag, "err", err)
				continue
			}
			for _, c := range expanded {
				cidrs[c] = struct{}{}
			}
		}
	}
	if len(cidrs) == 0 {
		if len(geoips) == 0 && len(ipCIDR) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("no ip_cidrs expanded")
	}
	out := make([]string, 0, len(cidrs))
	for c := range cidrs {
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

// decompileGeoip shells out to sing-box, reads the produced JSON, and
// extracts every ip_cidr from rules[].ip_cidr (also handles default_value-
// style top-level rule arrays).
//
// Source path priority:
//
//  1. srsCacheDir (when set, mihomo mode): ensure <tag>.srs is present,
//     fetching from MetaCubeX /sing/ branch on demand. Required because
//     mihomo's rule-sets dir holds .mrs which sing-box CLI can't decompile.
//  2. ruleSetsDir (sing-box mode): read <tag>.srs from there directly.
func (e *Expander) decompileGeoip(tag string) ([]string, error) {
	var srcPath string
	if e.srsCacheDir != "" {
		srcPath = filepath.Join(e.srsCacheDir, tag+".srs")
		if _, err := os.Stat(srcPath); err != nil {
			if err := e.fetchSrsToCache(tag, srcPath); err != nil {
				return nil, fmt.Errorf("fetch %s.srs: %w", tag, err)
			}
		}
	} else {
		srcPath = filepath.Join(e.ruleSetsDir, tag+".srs")
		if _, err := os.Stat(srcPath); err != nil {
			return nil, err
		}
	}
	// Use the cache dir's parent for scratch, NOT $TMPDIR — leap-gateway's
	// systemd unit sets ProtectSystem=strict with ReadWritePaths limited to
	// /var/lib/leap and /etc/leap, so /tmp is read-only. cachePath defaults
	// to /var/lib/leap/whitelist-resolved.json so its parent is writable.
	tmpParent := filepath.Dir(e.cachePath)
	if err := os.MkdirAll(tmpParent, 0o755); err != nil {
		return nil, err
	}
	tmpDir, err := os.MkdirTemp(tmpParent, "leap-srs-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)
	outPath := filepath.Join(tmpDir, tag+".json")

	cmd := exec.Command(e.singboxBin, "rule-set", "decompile", "-o", outPath, srcPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("sing-box decompile %s: %w (%s)", tag, err, strings.TrimSpace(string(out)))
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Rules []struct {
			IPCIDR []string `json:"ip_cidr"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse decompiled %s: %w", tag, err)
	}
	var out []string
	for _, r := range doc.Rules {
		out = append(out, r.IPCIDR...)
	}
	return out, nil
}

// fetchSrsToCache downloads <tag>.srs from MetaCubeX /sing/ branch into
// dest. Used by decompileGeoip under mihomo mode where the engine's
// rule-sets dir only has .mrs. Tries the proxy client first (routes
// through the local data plane to egress past the GFW), falls back to
// direct on connection error.
func (e *Expander) fetchSrsToCache(tag, dest string) error {
	if e.srsHTTPClient == nil {
		return fmt.Errorf("srs cache not configured (call WithMihomoSrsCache)")
	}
	stem := strings.TrimPrefix(tag, "geoip-")
	stem = strings.TrimPrefix(stem, "geosite-")
	url := "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/sing/geo/geoip/" + stem + ".srs"
	if strings.HasPrefix(tag, "geosite-") {
		url = "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/sing/geo/geosite/" + stem + ".srs"
	}

	body, err := e.fetchSrsOnce(e.srsHTTPClient, url)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	slog.Info("whitelistexpand: cached .srs for geoip expansion", "tag", tag, "path", dest)
	return nil
}

func (e *Expander) fetchSrsOnce(c *http.Client, srsURL string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, srsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "leap-gateway/whitelistexpand")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("srs fetch %s: HTTP %d", srsURL, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

func (e *Expander) markStale(msg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.snap == nil {
		return
	}
	e.snap.Stale = true
	e.snap.LastError = msg
}

func (e *Expander) saveToDisk(s *Snapshot) error {
	if err := os.MkdirAll(filepath.Dir(e.cachePath), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := e.cachePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, e.cachePath)
}

// parseGeosite recursively walks one v2fly category, following include:,
// stripping comments, and pruning unsupported types (regex/keyword) that
// don't translate to "FeiLian domain suffix" semantics.
func (e *Expander) parseGeosite(ctx context.Context, name string, seen map[string]struct{}, out map[string]struct{}) (string, error) {
	if _, ok := seen[name]; ok {
		return "", nil
	}
	seen[name] = struct{}{}

	body, source, err := e.fetch(ctx, name)
	if err != nil {
		return "", err
	}

	var sourceUsed = source
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Drop @attributes (e.g. "domain.com @cn @ads") — sing-box loads them
		// all by default, so we mirror that.
		head := strings.TrimSpace(strings.SplitN(line, "@", 2)[0])
		if head == "" {
			continue
		}
		switch {
		case strings.HasPrefix(head, "include:"):
			child := strings.TrimSpace(strings.TrimPrefix(head, "include:"))
			if child != "" {
				if _, err := e.parseGeosite(ctx, child, seen, out); err != nil {
					slog.Warn("whitelistexpand: include failed", "child", child, "err", err)
				}
			}
		case strings.HasPrefix(head, "regex:"), strings.HasPrefix(head, "keyword:"):
			// FeiLian domain whitelist doesn't take regex/keyword. Skip.
		case strings.HasPrefix(head, "full:"):
			out[strings.ToLower(strings.TrimPrefix(head, "full:"))] = struct{}{}
		case strings.HasPrefix(head, "domain:"):
			out[strings.ToLower(strings.TrimPrefix(head, "domain:"))] = struct{}{}
		default:
			out[strings.ToLower(head)] = struct{}{}
		}
	}
	return sourceUsed, nil
}

// fetch tries each source in order, returning the first successful body
// along with which base URL was used (for telemetry).
func (e *Expander) fetch(ctx context.Context, name string) (string, string, error) {
	var lastErr error
	for _, base := range e.sources {
		url := base + name
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			lastErr = err
			continue
		}
		resp, err := e.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode/100 != 2 {
			lastErr = fmt.Errorf("%s: status %d", url, resp.StatusCode)
			continue
		}
		return string(body), base, nil
	}
	return "", "", lastErr
}
