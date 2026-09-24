package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
	"github.com/leap-gateway/leap-gateway/internal/dataplane"
	"github.com/leap-gateway/leap-gateway/internal/subscribe"
)

// blockingRenderer is a Renderer whose Write parks until release is closed,
// so a test can hold a refresh open and observe what a second request or a
// cancelled client does meanwhile. Write is the right place to block: it
// sits between Refresh and Reload, which is exactly the window where a
// cancelled context used to leave the new config on disk unreloaded.
type blockingRenderer struct {
	release chan struct{}
	entered chan struct{}
	writes  atomic.Int32
}

func newBlockingRenderer() *blockingRenderer {
	return &blockingRenderer{
		release: make(chan struct{}),
		entered: make(chan struct{}, 8),
	}
}

func (b *blockingRenderer) Path() string { return "/tmp/leap-test-config.yaml" }

func (b *blockingRenderer) Write(_ []subscribe.Outbound) ([]byte, error) {
	b.writes.Add(1)
	b.entered <- struct{}{}
	<-b.release
	return []byte("rendered"), nil
}

func (b *blockingRenderer) RenderOnly(_ []subscribe.Outbound) ([]byte, error) {
	return []byte("rendered"), nil
}
func (b *blockingRenderer) SetSubscriptions(_ []config.SubscriptionEntry)   {}
func (b *blockingRenderer) SetWhitelist(_ string, _ config.WhitelistConfig) {}
func (b *blockingRenderer) SetFakeIPSkip(_ []string)                        {}
func (b *blockingRenderer) AssignmentForIP(_ string, _ []subscribe.Outbound) ([]string, bool) {
	return nil, false
}

// refreshFixture wires a Server with no subscriptions (so Refresh is a
// no-op), a blocking renderer, and a Controller pointed at a stub clash-api
// so Reload takes the hot-reload path and never shells out to systemctl.
func refreshFixture(t *testing.T) (*Server, *blockingRenderer, *atomic.Int32) {
	t.Helper()
	var reloads atomic.Int32
	clash := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/configs" {
			reloads.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(clash.Close)

	rend := newBlockingRenderer()
	s := &Server{deps: Deps{
		Subscribe:  subscribe.NewManagerWithFetch(nil, time.Second, "test-ua"),
		Renderer:   rend,
		Controller: dataplane.NewController(config.ClashAPIConfig{ExternalController: clash.Listener.Addr().String()}),
	}}
	return s, rend, &reloads
}

// TestRefresh_SurvivesClientCancel is the regression test for 2026-09-24:
// a client disconnect mid-refresh used to cancel the data-plane reload,
// leaving the freshly rendered config on disk while mihomo kept serving
// the old one (and the systemctl fallback failed on the same dead context,
// so nothing recovered it).
func TestRefresh_SurvivesClientCancel(t *testing.T) {
	s, rend, reloads := refreshFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/subscribe/refresh", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleRefresh(rec, req)
	}()

	// Wait until the refresh is parked inside Write, then kill the client.
	select {
	case <-rend.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh never reached Renderer.Write")
	}
	cancel()

	// Give the cancellation a moment to propagate before releasing, so a
	// ctx-bound reload would definitely have observed it.
	time.Sleep(50 * time.Millisecond)
	close(rend.release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleRefresh did not return")
	}

	if got := reloads.Load(); got != 1 {
		t.Errorf("reloads = %d, want 1 — the data-plane reload must survive a client disconnect", got)
	}
}

// TestRefresh_SecondIsRejected covers the state that the WithoutCancel fix
// newly made reachable: round one now outlives the disconnect that prompted
// the operator to reload the page and click again, so two refreshes can run
// at once. Renderer.Write stages through one fixed `.tmp` path with no lock,
// so the second must be refused rather than allowed to interleave.
func TestRefresh_SecondIsRejected(t *testing.T) {
	s, rend, _ := refreshFixture(t)

	rec1 := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleRefresh(rec1, httptest.NewRequest(http.MethodPost, "/api/subscribe/refresh", nil))
	}()

	select {
	case <-rend.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first refresh never reached Renderer.Write")
	}

	// First refresh is in flight. The second must be refused immediately.
	// Run it off-goroutine with a deadline: without the single-flight guard
	// it does not merely return the wrong status, it parks in Write behind
	// the same unclosed release channel, so a direct call would deadlock
	// the test instead of reporting a failure.
	rec2 := httptest.NewRecorder()
	second := make(chan struct{})
	go func() {
		defer close(second)
		s.handleRefresh(rec2, httptest.NewRequest(http.MethodPost, "/api/subscribe/refresh", nil))
	}()
	select {
	case <-second:
		if rec2.Code != http.StatusConflict {
			t.Errorf("second refresh status = %d, want %d", rec2.Code, http.StatusConflict)
		}
	case <-time.After(2 * time.Second):
		t.Error("second refresh blocked instead of being refused — single-flight guard missing")
	}

	close(rend.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("first handleRefresh did not return")
	}

	if rec1.Code != http.StatusOK {
		t.Errorf("first refresh status = %d, want %d", rec1.Code, http.StatusOK)
	}
	if got := rend.writes.Load(); got != 1 {
		t.Errorf("Renderer.Write calls = %d, want 1 (single-flight let a second render through)", got)
	}
}

// TestRefresh_FlagClearsAfterCompletion guards the obvious way to get the
// single-flight wrong: leaking the flag would wedge the endpoint at 409 for
// the process lifetime.
func TestRefresh_FlagClearsAfterCompletion(t *testing.T) {
	s, rend, reloads := refreshFixture(t)
	close(rend.release) // never block

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		s.handleRefresh(rec, httptest.NewRequest(http.MethodPost, "/api/subscribe/refresh", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("refresh %d status = %d, want %d", i+1, rec.Code, http.StatusOK)
		}
	}
	if got := reloads.Load(); got != 2 {
		t.Errorf("reloads = %d, want 2", got)
	}
}

// TestHandlerGoroutineNeedsDetachedContext pins the contract that
// handlePoolRollback depends on. It does not exercise that handler (it needs
// a live *nodescorer.Scorer); it locks down the net/http behavior that made
// its background re-render a permanent no-op: r.Context() is cancelled the
// moment the handler returns, so a goroutine outliving the handler must be
// handed a detached context or it has none.
//
// Keep this test if someone proposes "simplifying" WithoutCancel back out of
// pool_transitions.go — the raw-r.Context() half is what shipped, and it
// never once completed a reload.
func TestHandlerGoroutineNeedsDetachedContext(t *testing.T) {
	type result struct {
		raw      error
		detached error
	}
	got := make(chan result, 1)

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		detached := context.WithoutCancel(r.Context())
		go func() {
			// Sleep so the handler has certainly returned before we look.
			time.Sleep(50 * time.Millisecond)
			got <- result{raw: r.Context().Err(), detached: detached.Err()}
		}()
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	r := <-got
	if r.raw == nil {
		t.Error("r.Context() still live after handler returned — premise of the rollback fix is wrong")
	}
	if r.detached != nil {
		t.Errorf("detached ctx err = %v, want nil — WithoutCancel must outlive the handler", r.detached)
	}
}
