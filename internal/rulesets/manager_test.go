package rulesets

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEmbeddedCatalogParses(t *testing.T) {
	t.Parallel()
	m, err := New(t.TempDir(), 5*time.Second, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cat := m.Catalog()
	if got := len(cat.Geosites); got < 30 {
		t.Errorf("geosites = %d, want >=30", got)
	}
	if got := len(cat.Geoips); got < 5 {
		t.Errorf("geoips = %d, want >=5", got)
	}
	for _, it := range cat.Geosites {
		if !strings.HasPrefix(it.Name, "geosite-") {
			t.Errorf("geosite item %q missing geosite- prefix", it.Name)
		}
		if !strings.Contains(it.URL, it.Name+".srs") {
			t.Errorf("geosite %q url %q does not embed name", it.Name, it.URL)
		}
	}
	for _, it := range cat.Geoips {
		if !strings.HasPrefix(it.Name, "geoip-") {
			t.Errorf("geoip item %q missing geoip- prefix", it.Name)
		}
	}
	if len(cat.DomainSuffixExamples) == 0 {
		t.Error("DomainSuffixExamples empty")
	}
	if len(cat.IPCIDRExamples) == 0 {
		t.Error("IPCIDRExamples empty")
	}
}

func TestEnsureInstalled_HappyPath(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("\x00FAKE-SRS-PAYLOAD"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	m := newTestManager(t, dir, "geosite-test", srv.URL+"/{name}.srs")

	if err := m.EnsureInstalled(context.Background(), "geosite-test"); err != nil {
		t.Fatalf("EnsureInstalled: %v", err)
	}
	if !m.IsInstalled("geosite-test") {
		t.Fatal("file not on disk after EnsureInstalled")
	}
	body, _ := os.ReadFile(filepath.Join(dir, "geosite-test.srs"))
	if string(body) != "\x00FAKE-SRS-PAYLOAD" {
		t.Errorf("on-disk body = %q", body)
	}

	// Idempotent: second call must not hit upstream.
	if err := m.EnsureInstalled(context.Background(), "geosite-test"); err != nil {
		t.Fatalf("second EnsureInstalled: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("upstream hits = %d, want 1", got)
	}
}

func TestEnsureInstalled_UpstreamError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	dir := t.TempDir()
	m := newTestManager(t, dir, "geosite-bad", srv.URL+"/{name}.srs")

	err := m.EnsureInstalled(context.Background(), "geosite-bad")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// No tmp or final left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir not empty after fetch failure: %v", names)
	}
}

func TestEnsureInstalled_NotInCatalog(t *testing.T) {
	t.Parallel()
	m, err := New(t.TempDir(), time.Second, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = m.EnsureInstalled(context.Background(), "geosite-bogus-xyz")
	if err == nil || !strings.Contains(err.Error(), "not in catalog") {
		t.Errorf("err = %v, want 'not in catalog'", err)
	}
}

func TestEnsureInstalled_ConcurrentDedupe(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	// Slow handler so all goroutines pile up before the first one finishes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(80 * time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	m := newTestManager(t, dir, "geosite-race", srv.URL+"/{name}.srs")

	const N = 10
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = m.EnsureInstalled(context.Background(), "geosite-race")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("upstream hits = %d, want exactly 1", got)
	}
}

// newTestManager builds a Manager whose items map contains exactly one entry
// pointing at the test server's URL template. Bypasses the embedded catalog
// so tests don't talk to the real github.
func newTestManager(t *testing.T, dir, name, urlTemplate string) *Manager {
	t.Helper()
	m, err := New(dir, 5*time.Second, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	url := strings.ReplaceAll(urlTemplate, "{name}", name)
	m.items = map[string]Item{name: {Name: name, URL: url, Category: "test"}}
	return m
}
