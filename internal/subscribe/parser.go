package subscribe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

// Manager owns subscriptions and the latest aggregated outbound list.
type Manager struct {
	entries []config.SubscriptionEntry
	fetcher *fetcher
	// globalUA is the configured Subscribe.UserAgent — captured here so the
	// UA-discovery fallback in Refresh can decide which alternate UA to try
	// without re-reading config every time.
	globalUA string

	mu            sync.RWMutex
	lastResults   []SubscriptionResult
	lastRefreshed time.Time

	// onUADiscovered is called (synchronously, off the refresh goroutine)
	// when Refresh's auto-fallback discovered a per-subscription UA that
	// works. The callback's job is to persist the discovery (e.g. write
	// back to gateway.yaml via configstore.Mutate) so future refreshes go
	// straight to the right UA without paying the discovery cost again.
	// Nil disables persistence — discovery still works in-memory.
	onUADiscovered func(name, ua string)
}

func NewManager(entries []config.SubscriptionEntry) *Manager {
	return NewManagerWithFetch(entries, 30*time.Second, "ClashforWindows/0.20.39")
}

// NewManagerWithFetch lets callers thread http_timeout and user_agent from
// config — many subscription providers gate by UA, so the default
// "leap-gateway/0.1" gets connection-reset on some upstreams.
func NewManagerWithFetch(entries []config.SubscriptionEntry, timeout time.Duration, ua string) *Manager {
	return NewManagerWithFetchAndProxy(entries, timeout, ua, "")
}

// NewManagerWithFetchAndProxy is NewManagerWithFetch + proxyURL routing.
// Production wires this with the local mihomo / sing-box loopback HTTP
// inbound (LeapInternalProxyURL) so subscription fetches don't go through
// the host's resolver — which under fakeip mode returns 198.18.x.x for any
// non-CN domain, making direct dial fail. Caught in production 2026-06-05.
//
// Empty proxyURL → direct OS network (used by tests + standalone CLIs).
func NewManagerWithFetchAndProxy(entries []config.SubscriptionEntry, timeout time.Duration, ua, proxyURL string) *Manager {
	return &Manager{
		entries:  entries,
		fetcher:  newFetcherWithProxy(timeout, ua, proxyURL),
		globalUA: ua,
	}
}

// WithUADiscoveryCallback registers a callback invoked when Refresh's
// auto-fallback finds a working per-subscription UA. Returns the receiver
// for chaining at construction.
func (m *Manager) WithUADiscoveryCallback(cb func(name, ua string)) *Manager {
	m.mu.Lock()
	m.onUADiscovered = cb
	m.mu.Unlock()
	return m
}

// Refresh fetches every enabled subscription and parses it. Errors per
// subscription are logged but do not abort the whole refresh.
//
// UA auto-discovery: when an entry has no explicit UserAgent override
// (e.UserAgent == "") AND the first fetch with the global UA either
//   - returns HTTP 5xx (some airports answer "wrong UA" with 500 instead
//     of a clean 4xx — ash class), or
//   - succeeds with 0 parsed nodes (some airports return 200 + empty
//     proxies list for the wrong UA — yuyun class),
// Refresh retries once with the alternate UA family (Clash↔sing-box). If
// the retry yields nodes, the discovered UA is fired through the
// onUADiscovered callback so the caller can persist it to yaml; the
// happy-path fetch the next refresh round goes straight to the right UA.
func (m *Manager) Refresh(ctx context.Context) ([]SubscriptionResult, error) {
	results := make([]SubscriptionResult, 0, len(m.entries))
	var firstErr error
	for _, e := range m.entries {
		if !e.Enabled {
			continue
		}
		body, fetchErr := m.fetcher.GetWithUA(ctx, e.URL, e.UserAgent)
		var parsed SubscriptionResult
		var parseErr error
		if fetchErr == nil {
			parsed, parseErr = ParseBytes(e.Name, e.Format, body)
		}
		// Auto-fallback decision tree.
		retry := false
		if e.UserAgent == "" {
			if h := AsHTTPError(fetchErr); h != nil && h.Status >= 500 && h.Status < 600 {
				retry = true
			}
			if fetchErr == nil && parseErr == nil && len(parsed.Outbounds) == 0 {
				retry = true
			}
		}
		if retry {
			usedUA := e.UserAgent
			if usedUA == "" {
				usedUA = m.globalUA
			}
			altUA := alternateUA(usedUA)
			if altUA != "" && altUA != usedUA {
				body2, err2 := m.fetcher.GetWithUA(ctx, e.URL, altUA)
				if err2 == nil {
					if r2, perr := ParseBytes(e.Name, e.Format, body2); perr == nil && len(r2.Outbounds) > 0 {
						slog.Info("subscribe: discovered ua override",
							"name", e.Name, "ua", altUA, "primary_ua", usedUA,
							"primary_outcome", primaryOutcome(fetchErr, parseErr, len(parsed.Outbounds)))
						parsed = r2
						parseErr = nil
						fetchErr = nil
						if m.onUADiscovered != nil {
							m.onUADiscovered(e.Name, altUA)
						}
					}
				}
			}
		}
		if fetchErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("fetch %s: %w", e.Name, fetchErr)
			}
			continue
		}
		if parseErr != nil {
			if firstErr == nil {
				firstErr = parseErr
			}
			continue
		}
		results = append(results, parsed)
	}
	m.mu.Lock()
	m.lastResults = results
	m.lastRefreshed = time.Now()
	m.mu.Unlock()
	return results, firstErr
}

// alternateUA returns the "other" common subscription-client UA family for
// auto-fallback. Maps Clash-family ↔ sing-box. Returns "" when the input UA
// doesn't fit a known family — caller should not retry in that case.
func alternateUA(ua string) string {
	lc := strings.ToLower(ua)
	switch {
	case strings.Contains(lc, "sing-box"):
		return "ClashforWindows/0.20.39"
	case ua == "" ||
		strings.Contains(lc, "clash") ||
		strings.Contains(lc, "stash") ||
		strings.Contains(lc, "mihomo") ||
		strings.HasPrefix(lc, "leap-gateway"):
		return "sing-box/1.10.7"
	}
	return ""
}

// primaryOutcome turns the (fetchErr, parseErr, nodeCount) of the first
// attempt into a short string for telemetry on the discovery log line.
func primaryOutcome(fetchErr, parseErr error, n int) string {
	if h := AsHTTPError(fetchErr); h != nil {
		return fmt.Sprintf("http_%d", h.Status)
	}
	if fetchErr != nil {
		return "fetch_err"
	}
	if parseErr != nil {
		return "parse_err"
	}
	if n == 0 {
		return "zero_nodes"
	}
	return "ok"
}

func (m *Manager) Snapshot() ([]SubscriptionResult, time.Time) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastResults, m.lastRefreshed
}

// SetEntries swaps the subscription set the next Refresh will iterate. Used by
// the management API after CRUD edits — the underlying cfg.Subscriptions
// slice may have been reallocated, so we cannot rely on shared backing array.
func (m *Manager) SetEntries(entries []config.SubscriptionEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Defensive copy so external mutations to the caller's slice don't show
	// up mid-refresh.
	cp := make([]config.SubscriptionEntry, len(entries))
	copy(cp, entries)
	m.entries = cp
}

// ParseBytes runs the parse + filter + tag-prefix path on a body that the
// caller already has in memory — useful for offline replay (selftest), unit
// tests, and any path that doesn't go through the HTTP fetcher.
func ParseBytes(name, format string, body []byte) (SubscriptionResult, error) {
	out, fmtName, err := parseAuto(body, format)
	if err != nil {
		return SubscriptionResult{}, fmt.Errorf("parse %s: %w", name, err)
	}
	out = filterOutbounds(out)
	for i := range out {
		tag := out[i].Tag()
		if tag == "" {
			tag = fmt.Sprintf("node-%d", i)
		}
		out[i]["tag"] = name + "/" + tag
	}
	return SubscriptionResult{Name: name, Format: fmtName, Outbounds: out}, nil
}

// AllOutbounds returns a flat list of all outbounds from the last refresh.
func (m *Manager) AllOutbounds() []Outbound {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var all []Outbound
	for _, r := range m.lastResults {
		all = append(all, r.Outbounds...)
	}
	return all
}

func parseAuto(body []byte, hint string) ([]Outbound, string, error) {
	switch hint {
	case "clash":
		o, err := parseClash(body)
		return o, "clash", err
	case "singbox":
		o, err := parseSingBox(body)
		return o, "singbox", err
	case "uri":
		o, err := parseURIList(body)
		return o, "uri", err
	case "sip008":
		o, err := parseSIP008(body)
		return o, "sip008", err
	}
	return detectAndParse(body)
}

func detectAndParse(body []byte) ([]Outbound, string, error) {
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) == 0 {
		return nil, "", errors.New("empty body")
	}
	// 1. JSON: sing-box (has "outbounds") or SIP008 (array of {server,...}).
	if trimmed[0] == '{' || trimmed[0] == '[' {
		var probe any
		if err := json.Unmarshal([]byte(trimmed), &probe); err == nil {
			switch v := probe.(type) {
			case map[string]any:
				if _, ok := v["outbounds"]; ok {
					o, err := parseSingBox(body)
					return o, "singbox", err
				}
			case []any:
				o, err := parseSIP008(body)
				return o, "sip008", err
			}
		}
	}
	// 2. Clash YAML — looks for "proxies:" near the top.
	if strings.Contains(trimmed, "\nproxies:") || strings.HasPrefix(trimmed, "proxies:") {
		o, err := parseClash(body)
		return o, "clash", err
	}
	// 3. URI list — possibly base64-wrapped.
	if decoded, ok := tryBase64(trimmed); ok {
		trimmed = decoded
	}
	if hasURIScheme(trimmed) {
		o, err := parseURIList([]byte(trimmed))
		return o, "uri", err
	}
	return nil, "", errors.New("unknown subscription format")
}

func tryBase64(s string) (string, bool) {
	s = strings.TrimSpace(s)
	// Subscriptions typically use URL-safe base64 without padding.
	for _, dec := range []*base64.Encoding{
		base64.StdEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.RawURLEncoding,
	} {
		if b, err := dec.DecodeString(s); err == nil {
			out := string(b)
			if hasURIScheme(out) {
				return out, true
			}
		}
	}
	return "", false
}

func hasURIScheme(s string) bool {
	for _, scheme := range []string{"vmess://", "vless://", "trojan://", "ss://", "ssr://", "hysteria2://", "hy2://", "tuic://"} {
		if strings.Contains(s, scheme) {
			return true
		}
	}
	return false
}
