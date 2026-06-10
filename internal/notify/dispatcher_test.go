package notify

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

// TestDispatcherFlushSentinelDoesNotDrop pins the regression: the
// aggregation timer's __flush__ sentinel must trigger a batch flush, not
// be silently dropped by shouldDropLocked. Caught when info-tier
// auto_swap notifications were stuck in the batch and never delivered
// to Lark on 89.
func TestDispatcherFlushSentinelDoesNotDrop(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
		w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Enabled: true,
		Lark: config.LarkConfig{
			WebhookURL:        srv.URL,
			SignatureRequired: false,
		},
		LocalLogPath:            "", // disable file log for test
		DedupWindow:             0,  // no dedup
		AggregationWindow:       100 * time.Millisecond,
		PerNodeRateLimitPerHour: 0, // no rate limit
		RetryAttempts:           1,
	}
	n := New(cfg, "")
	defer n.Close()

	// Emit one info event. With aggregation_window=100ms, dispatcher
	// should set a timer that fires after 100ms and triggers flush.
	n.Emit(Event{
		Time:     time.Now(),
		Severity: SeverityInfo,
		Type:     "test",
		Subject:  "regression",
		Body:     "should reach the webhook",
	})

	// Wait long enough for aggregation + delivery + a bit of slack.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&hits) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected webhook hit (flush sentinel must wake the batch); got 0")
}

// TestLarkSign ensures HMAC signature matches Lark's spec: the secret
// goes inside the message-to-sign, not as the HMAC key. (Lark's docs
// are unusual on this; getting it backwards = "sign match fail".)
func TestLarkSign(t *testing.T) {
	got, err := signLark(1700000000, "mysecret")
	if err != nil {
		t.Fatalf("signLark: %v", err)
	}
	if got == "" {
		t.Error("signLark returned empty")
	}
	// Stable across runs — fixing the value pins the algorithm choice.
	// If the algorithm changes (key swap, encoding change), this catches it.
	want := "Jp33/xXhCipDEpjyHvEyc7mRSyXWHbNz6J8+C3qQKNo="
	if got != want {
		t.Errorf("signLark: got %q want %q", got, want)
	}
}

// TestRenderBatchHeader pins the current header semantics: urgent uses
// 🚨, all other severities (info / done) use ℹ. The deliberate
// simplification keeps the operator's eye drawn to actually-urgent
// items; "done" / "info" both mean "system did its job, FYI".
func TestRenderBatchHeader(t *testing.T) {
	got := renderBatch([]Event{{Severity: SeverityUrgent, Subject: "x"}})
	if !strings.HasPrefix(got, "🚨") {
		t.Errorf("urgent header missing 🚨: %q", got[:30])
	}
	got = renderBatch([]Event{{Severity: SeverityInfo, Subject: "x"}})
	if !strings.HasPrefix(got, "ℹ") {
		t.Errorf("info header missing ℹ: %q", got[:30])
	}
	got = renderBatch([]Event{{Severity: SeverityDone, Subject: "x"}})
	if !strings.HasPrefix(got, "ℹ") {
		t.Errorf("done header should also use ℹ (not urgent): %q", got[:30])
	}
}
