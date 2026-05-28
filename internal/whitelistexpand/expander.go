// Package whitelistexpand turns the WL config (geosite tags + domain_suffix)
// into a flat list of domain suffixes — the same expansion that
// scripts/expand-whitelist.py does, but as an in-process service so other
// tooling (e.g. FeiLian SaaS sync) can pull a current list via API instead
// of scraping the upstream itself.
//
// Source: v2fly/domain-list-community via jsdelivr CDN. The PoC node
// (192.168.70.92) was verified to reach jsdelivr in <1s and to time out on
// raw.githubusercontent — so jsdelivr is the primary, github raw is fallback.
//
// Cache strategy:
//   - In-memory snapshot served by the API.
//   - Disk persistence (/var/lib/leap/whitelist-domains.json) so a restart
//     doesn't immediately serve "no data" while the network round-trip
//     completes.
//   - Refresh is async. The API returns the last good snapshot with its
//     timestamp; callers can detect staleness.
package whitelistexpand

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultCachePath = "/var/lib/leap/whitelist-domains.json"
	httpTimeout      = 12 * time.Second
)

// Sources are tried in order. jsdelivr first because it's fast + un-blocked
// from CN; github raw is the fallback for rare CN-DNS-glitch moments.
var defaultSources = []string{
	"https://cdn.jsdelivr.net/gh/v2fly/domain-list-community@master/data/",
	"https://raw.githubusercontent.com/v2fly/domain-list-community/master/data/",
}

// Snapshot is what GET /api/whitelist/domains returns.
type Snapshot struct {
	Geosites     []string  `json:"geosites"`
	Geoips       []string  `json:"geoips"`
	DomainSuffix []string  `json:"domain_suffix"`
	IPCIDR       []string  `json:"ip_cidr"`
	Domains      []string  `json:"domains"`
	Count        int       `json:"count"`
	LastBuiltAt  time.Time `json:"last_built_at"`
	Source       string    `json:"source"`           // which CDN base actually served
	Stale        bool      `json:"stale,omitempty"`  // true when the latest refresh failed and we're serving an older snapshot
	LastError    string    `json:"last_error,omitempty"`
}

// Expander owns the cache + the build worker.
type Expander struct {
	cachePath string
	client    *http.Client
	sources   []string

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
		client:    &http.Client{Timeout: httpTimeout},
		sources:   defaultSources,
	}
}

// LoadFromDisk seeds the in-memory snapshot from the on-disk cache. Returns
// nil quietly when the file doesn't exist (first run after install).
func (e *Expander) LoadFromDisk() error {
	data, err := os.ReadFile(e.cachePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("decode %s: %w", e.cachePath, err)
	}
	e.mu.Lock()
	e.snap = &s
	e.mu.Unlock()
	slog.Info("whitelistexpand: loaded cache from disk",
		"path", e.cachePath, "count", s.Count, "built_at", s.LastBuiltAt)
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
	cp.Geosites = append([]string(nil), e.snap.Geosites...)
	cp.Geoips = append([]string(nil), e.snap.Geoips...)
	cp.DomainSuffix = append([]string(nil), e.snap.DomainSuffix...)
	cp.IPCIDR = append([]string(nil), e.snap.IPCIDR...)
	return &cp
}

// Refresh runs the expansion against the supplied WL inputs and atomically
// replaces the snapshot. On failure it preserves the previous snapshot but
// marks it stale + records the error.
//
// Geoips and ipCIDR are NOT expanded — they're passed through into Snapshot
// so callers see the full whitelist picture, but only domain-side entries
// (geosites + suffix) actually get walked against v2fly. FeiLian's "极速模式"
// is domain-only anyway, so the .Domains field still answers what FeiLian
// needs.
func (e *Expander) Refresh(ctx context.Context, geosites, geoips []string, suffix, ipCIDR []string) (*Snapshot, error) {
	e.buildMu.Lock()
	defer e.buildMu.Unlock()

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
			// Treat per-category failures as warnings, not fatal. A missing
			// upstream file (typo) shouldn't take down the whole refresh.
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
	if len(domains) == 0 {
		// Don't overwrite a previous good snapshot with empty just because
		// the entire upstream is unreachable for a moment.
		e.markStale("no domains expanded — upstream unreachable?")
		return e.Snapshot(), fmt.Errorf("expansion produced 0 domains")
	}

	out := make([]string, 0, len(domains))
	for d := range domains {
		out = append(out, d)
	}
	sort.Strings(out)

	snap := &Snapshot{
		Geosites:     append([]string(nil), geosites...),
		Geoips:       append([]string(nil), geoips...),
		DomainSuffix: append([]string(nil), suffix...),
		IPCIDR:       append([]string(nil), ipCIDR...),
		Domains:      out,
		Count:        len(out),
		LastBuiltAt:  time.Now().UTC(),
		Source:       sourceUsed,
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
		"categories", len(tags), "domains", snap.Count, "source", sourceUsed)
	return e.Snapshot(), nil
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
