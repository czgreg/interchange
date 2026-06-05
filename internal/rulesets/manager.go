// Package rulesets owns the geosite/geoip catalog (a static name → URL
// list shipped with the binary) and lazily fetches .srs blobs from the
// MetaCubeX/meta-rules-dat upstream into a local rule-sets directory the
// first time an operator selects them via the management API.
package rulesets

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/leaphttp"
)

//go:embed catalog.json
var rawCatalog []byte

// Item is a single catalog entry: tag name, fully-resolved upstream URL,
// and a free-form category string used by the API for UI grouping.
type Item struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Category string `json:"category"`
}

// Catalog is the resolved catalog presented to the API: each side already
// has its url_template applied. The Examples slices are static hints
// surfaced via /api/rule-sets so the UI can prefill domain_suffix / ip_cidr
// inputs.
type Catalog struct {
	Geosites             []Item
	Geoips               []Item
	DomainSuffixExamples []string
	IPCIDRExamples       []string
}

// Manager owns the catalog + a local on-disk cache directory. It serializes
// concurrent EnsureInstalled calls per-name so two PUTs that select the
// same uninstalled tag only trigger one HTTP fetch.
type Manager struct {
	dir     string
	catalog *Catalog
	items   map[string]Item
	httpc   *http.Client
	locks   sync.Map // name -> *sync.Mutex
	// engine controls upstream URL flavor + local file extension:
	//   "sing-box" (default) → .../sing/geo/<kind>/<stem>.srs   → <name>.srs on disk
	//   "mihomo"             → .../meta/geo/<kind>/<stem>.mrs   → <name>.mrs on disk
	// MetaCubeX/meta-rules-dat publishes both side by side.
	engine string
}

// New parses the embedded catalog and returns a Manager that writes
// downloaded .srs files into dir. httpTimeout caps each HTTP fetch.
//
// proxyURL is optional. When non-empty, fetches are routed through that
// HTTP proxy — used in production to relay through the local mihomo /
// sing-box's http inbound so .srs downloads transit the airport
// (raw.githubusercontent is GFW-blocked from inside CN). Empty string ⇒
// direct OS network.
//
// The HTTP client falls back to direct dial when the proxy is unreachable
// (data plane crash-looping or mid-restart) — matches the leaphttp
// pattern used by subscribe + whitelistexpand. Without fallback, a single
// data-plane outage would block all on-demand .srs fetches indefinitely.
//
// Engine defaults to "sing-box". Use WithEngine to switch to mihomo, which
// rewrites upstream URLs to MetaCubeX's meta/<kind>/<stem>.mrs path and
// stores files with the .mrs extension.
func New(dir string, httpTimeout time.Duration, proxyURL string) (*Manager, error) {
	cat, items, err := parseCatalog(rawCatalog)
	if err != nil {
		return nil, fmt.Errorf("rulesets: parse embedded catalog: %w", err)
	}
	return &Manager{
		dir:     dir,
		catalog: cat,
		items:   items,
		httpc:   leaphttp.NewClient(proxyURL, httpTimeout, "rulesets"),
		engine:  "sing-box",
	}, nil
}

// WithEngine sets which proxy engine the on-disk rule-sets target. "sing-box"
// (default) keeps the embedded catalog's .srs URLs verbatim. "mihomo" rewrites
// each fetch to MetaCubeX's .mrs publication on the same release branch.
//
// Must be called before any EnsureInstalled — calling after that races against
// inflight fetches.
func (m *Manager) WithEngine(engine string) *Manager {
	m.engine = engine
	return m
}

// Catalog returns the parsed catalog. The returned pointer is shared and
// must not be mutated by callers.
func (m *Manager) Catalog() *Catalog { return m.catalog }

// Lookup returns the catalog entry for name, or false if name is not in
// the catalog.
func (m *Manager) Lookup(name string) (Item, bool) {
	it, ok := m.items[name]
	return it, ok
}

// IsInstalled reports whether <name>.<ext> exists in the rule-sets directory.
// Extension follows the engine: .srs for sing-box, .mrs for mihomo.
func (m *Manager) IsInstalled(name string) bool {
	_, err := os.Stat(m.localPath(name))
	return err == nil
}

// localPath returns the on-disk path the manager writes <name>'s blob to.
// Engine-dependent: sing-box → .srs, mihomo → .mrs.
func (m *Manager) localPath(name string) string {
	return filepath.Join(m.dir, name+m.fileExt())
}

// fileExt returns ".srs" or ".mrs" per the configured engine.
func (m *Manager) fileExt() string {
	if m.engine == "mihomo" {
		return ".mrs"
	}
	return ".srs"
}

// rewriteURL maps a sing-box-flavored .srs URL to mihomo's .mrs publication.
// MetaCubeX/meta-rules-dat publishes both flavors at parallel paths:
//
//	sing/geo/<kind>/<stem>.srs   ← sing-box
//	meta/geo/<kind>/<stem>.mrs   ← mihomo
//
// For sing-box engine, returns the URL unchanged.
func (m *Manager) rewriteURL(orig string) string {
	if m.engine != "mihomo" {
		return orig
	}
	out := orig
	out = strings.Replace(out, "/sing/geo/", "/meta/geo/", 1)
	if strings.HasSuffix(out, ".srs") {
		out = strings.TrimSuffix(out, ".srs") + ".mrs"
	}
	return out
}

// EnsureInstalled returns nil if <name>.srs is already on disk; otherwise
// looks the name up in the catalog and downloads it. Concurrent calls for
// the same name are coalesced — only the first one fetches.
//
// Errors:
//   - name not in catalog → "rule-set %q not in catalog" (treat as 400)
//   - upstream non-200 / network failure → wrapped, treat as 502
func (m *Manager) EnsureInstalled(ctx context.Context, name string) error {
	if m.IsInstalled(name) {
		return nil
	}
	item, ok := m.items[name]
	if !ok {
		return fmt.Errorf("rule-set %q not in catalog", name)
	}

	val, _ := m.locks.LoadOrStore(name, &sync.Mutex{})
	mu := val.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	// Re-check after acquiring the lock — another goroutine may have just
	// downloaded it while we waited.
	if m.IsInstalled(name) {
		return nil
	}
	return m.fetch(ctx, item)
}

// EnsureInstalledFromURL behaves like EnsureInstalled but bypasses the
// catalog and uses the supplied URL. Used for infrastructure rule-sets
// (geosite-cn, geoip-cn) that aren't user-toggleable and have their URL
// pinned in gateway.yaml's singbox.route.{geosite_url,geoip_url}.
func (m *Manager) EnsureInstalledFromURL(ctx context.Context, name, url string) error {
	if m.IsInstalled(name) {
		return nil
	}
	val, _ := m.locks.LoadOrStore(name, &sync.Mutex{})
	mu := val.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	if m.IsInstalled(name) {
		return nil
	}
	return m.fetch(ctx, Item{Name: name, URL: url})
}

func (m *Manager) fetch(ctx context.Context, item Item) error {
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", m.dir, err)
	}

	fetchURL := m.rewriteURL(item.URL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return err
	}
	resp, err := m.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", fetchURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get %s: %s", fetchURL, resp.Status)
	}

	final := m.localPath(item.Name)
	tmp := final + ".tmp"

	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmp)
		}
	}()

	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("rename %s: %w", final, err)
	}
	removeTmp = false

	slog.Info("rulesets: installed", "name", item.Name, "url", fetchURL, "path", final)
	return nil
}

func parseCatalog(blob []byte) (*Catalog, map[string]Item, error) {
	var raw struct {
		Geosites struct {
			URLTemplate string         `json:"url_template"`
			Items       []rawCatItem   `json:"items"`
		} `json:"geosites"`
		Geoips struct {
			URLTemplate string         `json:"url_template"`
			Items       []rawCatItem   `json:"items"`
		} `json:"geoips"`
		Examples struct {
			DomainSuffix []string `json:"domain_suffix"`
			IPCIDR       []string `json:"ip_cidr"`
		} `json:"examples"`
	}
	if err := json.Unmarshal(blob, &raw); err != nil {
		return nil, nil, err
	}
	if !strings.Contains(raw.Geosites.URLTemplate, "{name}") && !strings.Contains(raw.Geosites.URLTemplate, "{stem}") {
		return nil, nil, fmt.Errorf("geosites.url_template missing {name} or {stem}")
	}
	if !strings.Contains(raw.Geoips.URLTemplate, "{name}") && !strings.Contains(raw.Geoips.URLTemplate, "{stem}") {
		return nil, nil, fmt.Errorf("geoips.url_template missing {name} or {stem}")
	}

	items := make(map[string]Item)
	expand := func(template string, src []rawCatItem) []Item {
		out := make([]Item, 0, len(src))
		for _, x := range src {
			// {stem} = name with the first "<kind>-" prefix stripped
			// (e.g. "geoip-google" → "google"). MetaCubeX-style upstream
			// paths are .../geo/{geosite,geoip}/<stem>.srs — the prefix
			// lives only in our internal tag identity, not the URL.
			stem := x.Name
			if i := strings.Index(x.Name, "-"); i >= 0 {
				stem = x.Name[i+1:]
			}
			url := strings.ReplaceAll(template, "{name}", x.Name)
			url = strings.ReplaceAll(url, "{stem}", stem)
			it := Item{Name: x.Name, URL: url, Category: x.Category}
			out = append(out, it)
			items[x.Name] = it
		}
		return out
	}
	cat := &Catalog{
		Geosites:             expand(raw.Geosites.URLTemplate, raw.Geosites.Items),
		Geoips:               expand(raw.Geoips.URLTemplate, raw.Geoips.Items),
		DomainSuffixExamples: append([]string{}, raw.Examples.DomainSuffix...),
		IPCIDRExamples:       append([]string{}, raw.Examples.IPCIDR...),
	}
	return cat, items, nil
}

type rawCatItem struct {
	Name     string `json:"name"`
	Category string `json:"category"`
}
