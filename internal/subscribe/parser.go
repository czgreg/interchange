package subscribe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

// Manager owns subscriptions and the latest aggregated outbound list.
type Manager struct {
	entries []config.SubscriptionEntry
	fetcher *fetcher

	mu            sync.RWMutex
	lastResults   []SubscriptionResult
	lastRefreshed time.Time
}

func NewManager(entries []config.SubscriptionEntry) *Manager {
	return NewManagerWithFetch(entries, 30*time.Second, "sing-box/1.8.0")
}

// NewManagerWithFetch lets callers thread http_timeout and user_agent from
// config — many subscription providers gate by UA, so the default
// "leap-gateway/0.1" gets connection-reset on some upstreams.
func NewManagerWithFetch(entries []config.SubscriptionEntry, timeout time.Duration, ua string) *Manager {
	return &Manager{
		entries: entries,
		fetcher: newFetcher(timeout, ua),
	}
}

// Refresh fetches every enabled subscription and parses it. Errors per
// subscription are logged but do not abort the whole refresh.
func (m *Manager) Refresh(ctx context.Context) ([]SubscriptionResult, error) {
	results := make([]SubscriptionResult, 0, len(m.entries))
	var firstErr error
	for _, e := range m.entries {
		if !e.Enabled {
			continue
		}
		body, err := m.fetcher.Get(ctx, e.URL)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("fetch %s: %w", e.Name, err)
			}
			continue
		}
		r, err := ParseBytes(e.Name, e.Format, body)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		results = append(results, r)
	}
	m.mu.Lock()
	m.lastResults = results
	m.lastRefreshed = time.Now()
	m.mu.Unlock()
	return results, firstErr
}

func (m *Manager) Snapshot() ([]SubscriptionResult, time.Time) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastResults, m.lastRefreshed
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
