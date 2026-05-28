package configstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

// TestMutateAtomicRoundtrip verifies the on-disk yaml after a Mutate parses
// back to the same struct content. Comments are NOT preserved (documented
// limitation).
func TestMutateAtomicRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	initial := []byte(`
api:
  listen: "127.0.0.1:18080"
subscribe:
  refresh_interval: 30m
subscriptions:
  - {name: yuyun, url: "https://example.com/sub?token=abc", format: auto, enabled: true}
`)
	if err := os.WriteFile(path, initial, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	store := New(path)

	if err := store.Mutate(cfg, func(c *config.Config) error {
		c.Subscriptions = append(c.Subscriptions, config.SubscriptionEntry{
			Name: "second", URL: "https://other.example/sub", Format: "auto", Enabled: true,
		})
		return nil
	}); err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	// Re-load from disk and verify.
	again, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load post-Mutate: %v", err)
	}
	if len(again.Subscriptions) != 2 {
		t.Errorf("want 2 subscriptions after Mutate, got %d", len(again.Subscriptions))
	}
	if again.Subscriptions[1].Name != "second" {
		t.Errorf("appended sub name = %q, want second", again.Subscriptions[1].Name)
	}
}

func TestMutateAtomicWriteFailureLeavesOriginal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	initial := []byte("api:\n  listen: \"127.0.0.1:18080\"\n")
	if err := os.WriteFile(path, initial, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(path)

	// Make the directory non-writable so the rename fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip("cannot chmod tempdir, skipping")
	}
	defer os.Chmod(dir, 0o755)

	store := New(path)
	err := store.Mutate(cfg, func(c *config.Config) error { return nil })
	if err == nil {
		t.Errorf("expected write failure, got nil")
	}
	// Original on disk should still be parseable.
	if _, err := config.Load(path); err != nil {
		t.Errorf("original yaml corrupted after failed Mutate: %v", err)
	}
}

// Sanity-check yaml round-trip emits parseable yaml even with all fields
// populated (catches schema-level errors like a non-marshalable type).
func TestRoundtripFullConfig(t *testing.T) {
	c := &config.Config{}
	c.SingBox.Route.Mode = "whitelist"
	c.SingBox.Route.Whitelist.Geosites = []config.GeositeRef{
		{Name: "geosite-google", URL: "https://example/google.srs"},
	}
	c.SingBox.Route.Whitelist.DomainSuffix = []string{"claude.ai"}
	c.SingBox.URLTest.Interval = 3 * time.Minute
	c.SingBox.URLTest.Watchdog.Enabled = true

	data, err := yaml.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var parsed config.Config
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Unmarshal back: %v", err)
	}
	if parsed.SingBox.Route.Mode != "whitelist" {
		t.Errorf("mode lost in round-trip: %q", parsed.SingBox.Route.Mode)
	}
	if len(parsed.SingBox.Route.Whitelist.Geosites) != 1 {
		t.Errorf("WL geosites lost in round-trip")
	}
}
