package subscribe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

// TestLastHealth_PerSubscriptionOutcomes reproduces the 2026-09-08 Hutao
// incident shape: five subscriptions, one 403s, four succeed.
//
// Before SubscriptionHealth existed the only observable signal was a single
// "subscribe refresh had errors" line carrying the FIRST error, and
// RunRefresh's zero-node warning fired only when EVERY subscription failed.
// One-of-five failing was therefore indistinguishable from a clean round at
// the alerting layer, and Hutao sat at 0 nodes for 17.5 hours.
func TestLastHealth_PerSubscriptionOutcomes(t *testing.T) {
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(minimalClash))
	}))
	defer okSrv.Close()
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer forbidden.Close()
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("this is not a subscription in any format"))
	}))
	defer garbage.Close()

	entries := []config.SubscriptionEntry{
		{Name: "good-1", URL: okSrv.URL, Format: "auto", Enabled: true, UserAgent: "clash/1"},
		{Name: "Hutao", URL: forbidden.URL, Format: "auto", Enabled: true, UserAgent: "clash/1"},
		{Name: "good-2", URL: okSrv.URL, Format: "auto", Enabled: true, UserAgent: "clash/1"},
		{Name: "broken", URL: garbage.URL, Format: "auto", Enabled: true, UserAgent: "clash/1"},
		{Name: "disabled", URL: okSrv.URL, Format: "auto", Enabled: false, UserAgent: "clash/1"},
	}
	m := NewManagerWithFetch(entries, 5*time.Second, "clash/1")

	results, err := m.Refresh(context.Background())
	if err == nil {
		t.Error("Refresh must still report a non-nil error when a subscription fails")
	}
	if len(results) != 2 {
		t.Errorf("results = %d, want 2 (only the healthy ones)", len(results))
	}

	health := m.LastHealth()
	// Disabled entries are skipped entirely — health covers ENABLED only.
	if len(health) != 4 {
		t.Fatalf("health = %d entries, want 4 (enabled only): %+v", len(health), health)
	}

	byName := map[string]SubscriptionHealth{}
	for _, h := range health {
		byName[h.Name] = h
	}

	if h := byName["good-1"]; h.Outcome != "ok" || h.NodeCount != 1 {
		t.Errorf("good-1 = %+v, want ok/1", h)
	}
	if h := byName["Hutao"]; h.Outcome != "fetch_error" {
		t.Errorf("Hutao = %+v, want fetch_error", h)
	} else if h.Err == "" {
		t.Error("Hutao health must carry the error detail, not just the outcome")
	} else if h.NodeCount != 0 {
		t.Errorf("Hutao NodeCount = %d, want 0", h.NodeCount)
	}
	if h := byName["broken"]; h.Outcome != "parse_error" {
		t.Errorf("broken = %+v, want parse_error", h)
	}
	if _, has := byName["disabled"]; has {
		t.Error("disabled subscription must not appear in health")
	}
}

// TestLastHealth_ZeroNodesIsNotOk pins the distinction that makes the alert
// actionable: a 200 + empty proxies list is NOT a healthy round. The
// upstream answered, so it is UA gating, an emptied subscription, or a
// revoked token — all needing an operator, none self-healing.
//
// UserAgent is set explicitly so the UA-discovery fallback does not fire
// (it only triggers when the entry has no operator-set UA).
func TestLastHealth_ZeroNodesIsNotOk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(emptyClash))
	}))
	defer srv.Close()

	m := NewManagerWithFetch([]config.SubscriptionEntry{
		{Name: "gated", URL: srv.URL, Format: "auto", Enabled: true, UserAgent: "clash/1"},
	}, 5*time.Second, "clash/1")

	if _, err := m.Refresh(context.Background()); err != nil {
		t.Fatalf("zero nodes is not a fetch/parse error: %v", err)
	}
	health := m.LastHealth()
	if len(health) != 1 {
		t.Fatalf("health = %d, want 1", len(health))
	}
	if health[0].Outcome != "zero_nodes" {
		t.Errorf("outcome = %q, want zero_nodes", health[0].Outcome)
	}
	if health[0].NodeCount != 0 {
		t.Errorf("NodeCount = %d, want 0", health[0].NodeCount)
	}
}

// TestLastHealth_AllOkNoFalsePositives guards the other direction: a clean
// round must produce no unhealthy entries, otherwise the alert becomes noise
// the operator learns to ignore.
func TestLastHealth_AllOkNoFalsePositives(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(minimalClash))
	}))
	defer srv.Close()

	m := NewManagerWithFetch([]config.SubscriptionEntry{
		{Name: "a", URL: srv.URL, Format: "auto", Enabled: true, UserAgent: "clash/1"},
		{Name: "b", URL: srv.URL, Format: "auto", Enabled: true, UserAgent: "clash/1"},
	}, 5*time.Second, "clash/1")

	if _, err := m.Refresh(context.Background()); err != nil {
		t.Fatalf("clean round returned error: %v", err)
	}
	for _, h := range m.LastHealth() {
		if h.Outcome != "ok" {
			t.Errorf("%s = %+v, want ok", h.Name, h)
		}
	}
}
