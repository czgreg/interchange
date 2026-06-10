// Package notify is the operator-notification subsystem. It accepts
// structured events from the rest of the gateway (scorer, subscription
// manager, etc.) and forwards them to operator-facing channels: Lark/Feishu
// webhook today, more channels later.
//
// Three guarantees this package provides on top of "POST to webhook":
//
//  1. Local persistence: every event is appended to a JSONL log on disk
//     BEFORE the webhook call. If Lark / network is down, the operator
//     can still reconstruct what happened from the log. Webhook delivery
//     becomes a best-effort optimization, not a single point of failure.
//
//  2. Dedup + rate limit: same (node, event_type) inside a short window
//     coalesces into one notification. Per-node-per-hour rate limit on
//     "info"-tier events. "urgent" events bypass everything.
//
//  3. HMAC signing for Lark when the operator has configured a secret
//     (Notifications.Lark.SignatureRequired). Required for production —
//     keeps the webhook channel from being abused if the URL leaks.
//
// The Notifier itself is goroutine-safe; emit calls are non-blocking
// (events are queued and processed by a single dispatcher goroutine).
package notify

import (
	"sync"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

// Severity classifies an event's urgency. The dispatcher applies different
// dedup / rate-limit / aggregation rules per severity:
//
//   urgent — never deduped, never aggregated, never rate-limited; retried
//            until delivered (best-effort across scoring rounds).
//   info   — deduped within DedupWindow, rate-limited per node per hour,
//            may be aggregated with other info events into one message.
//   done   — same handling as info; used for "swap completed" confirmations.
type Severity string

const (
	SeverityUrgent Severity = "urgent"
	SeverityInfo   Severity = "info"
	SeverityDone   Severity = "done"
)

// Event is one operator-facing notification before formatting / delivery.
// Subject is the short headline (becomes Lark message title); Body is the
// detail block; Metadata is structured fields surfaced both in the Lark
// card AND the local JSONL log for downstream tooling.
type Event struct {
	Time     time.Time
	Severity Severity
	Type     string // e.g. "node_dead", "pool_changed", "emergency_evict"
	Node     string // empty for pool-level / system events
	Subject  string
	Body     string
	Metadata map[string]any
}

// Notifier is the public face of the package. Construct via New, then
// call Emit from anywhere that produces events. Close on shutdown.
type Notifier struct {
	cfg    config.NotificationsConfig
	secret string // runtime-only Lark signing secret; not persisted

	mu       sync.Mutex
	dedup    map[string]time.Time // key = node|type, value = last emit time
	rateLog  map[string][]time.Time // per-node info emit timestamps in last hour

	// outbound Lark client (lazily inited); local file logger
	lark    *larkClient
	fileLog *fileLogger

	queue chan Event
	stop  chan struct{}
	done  chan struct{}
}

// New constructs a Notifier with the given config. Secret is the runtime-
// only Lark signing secret (set via API; never read from yaml). New
// always succeeds — even with notifications disabled, callers can Emit
// safely (events are dropped silently in that case, after JSONL log if
// the local log path is writable).
func New(cfg config.NotificationsConfig, secret string) *Notifier {
	n := &Notifier{
		cfg:     cfg,
		secret:  secret,
		dedup:   map[string]time.Time{},
		rateLog: map[string][]time.Time{},
		queue:   make(chan Event, 256),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if cfg.LocalLogPath != "" {
		n.fileLog = newFileLogger(cfg.LocalLogPath)
	}
	if cfg.Enabled && cfg.Lark.WebhookURL != "" {
		n.lark = newLarkClient(cfg.Lark, secret, cfg.RetryAttempts)
	}
	go n.run()
	return n
}

// Configure replaces the Lark webhook URL + secret atomically. Setting a
// non-empty URL also implicitly enables the notifier (calling this
// endpoint is itself the operator opt-in signal — they wouldn't be
// configuring a webhook if they didn't want notifications). Empty URL
// disables the Lark channel but leaves the local JSONL log enabled.
// Returns the post-configuration status.
func (n *Notifier) Configure(webhookURL, secret string, signatureRequired bool) Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.cfg.Lark.WebhookURL = webhookURL
	n.cfg.Lark.SignatureRequired = signatureRequired
	n.secret = secret
	if webhookURL != "" {
		// Configuring a webhook = explicit operator opt-in.
		n.cfg.Enabled = true
		n.lark = newLarkClient(n.cfg.Lark, secret, n.cfg.RetryAttempts)
	} else {
		// Empty URL = remove the Lark channel. Don't toggle global
		// Enabled — operator might still want local-log-only mode.
		n.lark = nil
	}
	return n.statusLocked()
}

// Status is the public-facing notifier health summary. Surfaced by
// /api/notifications/status. Never includes the secret itself — only
// whether one is configured.
type Status struct {
	Enabled            bool   `json:"enabled"`
	WebhookConfigured  bool   `json:"webhook_configured"`
	SignatureRequired  bool   `json:"signature_required"`
	SecretConfigured   bool   `json:"secret_configured"`
	LocalLogPath       string `json:"local_log_path"`
	DedupWindowSec     int    `json:"dedup_window_sec"`
	AggregationWindowSec int  `json:"aggregation_window_sec"`
	PerNodeRateLimitPerHour int `json:"per_node_rate_limit_per_hour"`
	RetryAttempts      int    `json:"retry_attempts"`
}

// GetStatus returns a snapshot of the notifier configuration. Safe to
// call concurrently.
func (n *Notifier) GetStatus() Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.statusLocked()
}

func (n *Notifier) statusLocked() Status {
	return Status{
		Enabled:                 n.cfg.Enabled,
		WebhookConfigured:       n.cfg.Lark.WebhookURL != "",
		SignatureRequired:       n.cfg.Lark.SignatureRequired,
		SecretConfigured:        n.secret != "",
		LocalLogPath:            n.cfg.LocalLogPath,
		DedupWindowSec:          int(n.cfg.DedupWindow.Seconds()),
		AggregationWindowSec:    int(n.cfg.AggregationWindow.Seconds()),
		PerNodeRateLimitPerHour: n.cfg.PerNodeRateLimitPerHour,
		RetryAttempts:           n.cfg.RetryAttempts,
	}
}

// Emit queues an event for processing. Non-blocking; if the queue is
// full the event is dropped (and logged via the file-only path if
// available). Callers should NOT block on Emit — they're typically the
// scorer's hot path.
//
// An empty Event (Type=="" — used by main.go's transition formatter
// to opt out of certain transition kinds, e.g. bootstrap) is silently
// dropped: not queued, not logged. Lets formatters return a no-op
// without coupling them to dispatcher internals.
func (n *Notifier) Emit(ev Event) {
	if ev.Type == "" {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	if ev.Severity == "" {
		ev.Severity = SeverityInfo
	}
	select {
	case n.queue <- ev:
	default:
		// Queue full — best-effort log to file synchronously, drop webhook.
		if n.fileLog != nil {
			n.fileLog.Append(ev, "queue_full", nil)
		}
	}
}

// Close stops the dispatcher goroutine, drains queued events, and closes
// the file log. Safe to call multiple times.
func (n *Notifier) Close() {
	select {
	case <-n.stop:
		return
	default:
	}
	close(n.stop)
	<-n.done
	if n.fileLog != nil {
		n.fileLog.Close()
	}
}

// TestSend dispatches a synthetic event synchronously and reports the
// delivery result. Used by /api/notifications/test so ops can verify
// webhook + signing config end-to-end after editing them. Bypasses
// dedup / rate-limit / aggregation — a test always tries to fire.
func (n *Notifier) TestSend() error {
	n.mu.Lock()
	lark := n.lark
	enabled := n.cfg.Enabled
	urlSet := n.cfg.Lark.WebhookURL != ""
	n.mu.Unlock()
	if !enabled {
		return errSimple("notifications disabled in config (notifications.enabled=false)")
	}
	if !urlSet || lark == nil {
		return errSimple("no Lark webhook configured (POST /api/notifications/lark)")
	}
	ev := Event{
		Time:     time.Now(),
		Severity: SeverityInfo,
		Type:     "test",
		Subject:  "leap-gateway notification test",
		Body:     "If you see this, the Lark webhook + signing config are working.",
	}
	if n.fileLog != nil {
		n.fileLog.Append(ev, "test_attempt", nil)
	}
	if err := lark.Send([]Event{ev}); err != nil {
		if n.fileLog != nil {
			n.fileLog.Append(ev, "test_failed", map[string]any{"err": err.Error()})
		}
		return err
	}
	if n.fileLog != nil {
		n.fileLog.Append(ev, "test_delivered", nil)
	}
	return nil
}

// Recent returns up to limit recent JSONL entries from the local log.
// Returns nil when no log path is configured or the file is missing.
func (n *Notifier) Recent(limit int) ([]map[string]any, error) {
	if n.fileLog == nil {
		return nil, nil
	}
	return n.fileLog.Tail(limit)
}

type errSimple string

func (e errSimple) Error() string { return string(e) }
