// log.go — local JSONL audit log of every notification attempt.
//
// One line per attempt (queued / dropped_dedup_or_rate_limit /
// webhook_failed / delivered). Always written BEFORE the webhook fires
// so a hard crash mid-delivery still leaves evidence on disk. ops can
// reconstruct "what was the system trying to tell me" without depending
// on Lark uptime.
//
// Append-only, buffered (4 KiB) with O_APPEND so concurrent writes from
// multiple processes (we don't currently have any, but it's cheap
// future-proofing) interleave at line boundaries instead of corrupting.
//
// No rotation in v1 — operator should logrotate or truncate
// /var/lib/leap/notifications.log periodically. File typically grows
// at ~100 lines/day on a healthy node, ~10 KiB/day = manageable.

package notify

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type fileLogger struct {
	path string
	mu   sync.Mutex
	f    *os.File
}

// notificationLogEntry is one JSONL row: the event itself plus the
// outcome the dispatcher recorded for it.
type notificationLogEntry struct {
	At       time.Time              `json:"at"`
	Outcome  string                 `json:"outcome"` // queued | delivered | webhook_failed | dropped_*
	Severity Severity               `json:"severity"`
	Type     string                 `json:"type"`
	Node     string                 `json:"node,omitempty"`
	Subject  string                 `json:"subject,omitempty"`
	Body     string                 `json:"body,omitempty"`
	Metadata map[string]any         `json:"metadata,omitempty"`
	Extra    map[string]any         `json:"extra,omitempty"` // dispatcher-side context (err, etc.)
}

func newFileLogger(path string) *fileLogger {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Warn("notify: cannot mkdir for local log", "path", path, "err", err)
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Warn("notify: cannot open local log", "path", path, "err", err)
		return nil
	}
	return &fileLogger{path: path, f: f}
}

// Append writes one entry. extra carries dispatcher-side context like
// retry error or queue-full reason. Failures are logged via slog and
// the entry is dropped — disk full / permissions are fatal-to-this-event
// but not to the gateway.
func (l *fileLogger) Append(ev Event, outcome string, extra map[string]any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := notificationLogEntry{
		At:       time.Now().UTC(),
		Outcome:  outcome,
		Severity: ev.Severity,
		Type:     ev.Type,
		Node:     ev.Node,
		Subject:  ev.Subject,
		Body:     ev.Body,
		Metadata: ev.Metadata,
		Extra:    extra,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		slog.Warn("notify: marshal log entry failed", "err", err)
		return
	}
	data = append(data, '\n')
	if _, err := l.f.Write(data); err != nil {
		slog.Warn("notify: write local log failed", "path", l.path, "err", err)
	}
}

// Close flushes and closes the file. Idempotent.
func (l *fileLogger) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}

// Tail returns up to limit most-recent JSONL entries from the log file.
// Reads the entire file (cheap given typical 10s-of-KB log sizes) and
// returns the last N lines parsed as map[string]any. Lines that fail
// to parse are skipped silently. Used by /api/notifications/recent.
func (l *fileLogger) Tail(limit int) ([]map[string]any, error) {
	if l == nil || limit <= 0 {
		return nil, nil
	}
	data, err := os.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	// Split into lines, keep the last `limit` non-empty.
	var entries []map[string]any
	start := 0
	for i, b := range data {
		if b != '\n' {
			continue
		}
		if i > start {
			var m map[string]any
			if json.Unmarshal(data[start:i], &m) == nil {
				entries = append(entries, m)
			}
		}
		start = i + 1
	}
	// Tail
	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries, nil
}
