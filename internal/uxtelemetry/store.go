// Package uxtelemetry collects user-perceived experience metrics from
// employee browsers and IDE plugins. Without this data, leap-gateway
// optimizes blindly: machine-side metrics (RTT, throughput, alive flags)
// say nothing about whether the actual person staring at Claude or
// ChatGPT is happy.
//
// Events arrive at /api/ux-telemetry from a thin client-side probe (browser
// extension or IDE plugin); the gateway stores them in a ring buffer and
// exposes raw + aggregated views. Storage is in-memory only — survival
// across restart is not a goal here. Operators reading the data should
// scrape /api/ux-telemetry/summary periodically into a longer-term store.
//
// Schema is intentionally narrow: every event captures one observation
// (domain, event_type, duration_ms, optional client_id). Resist the urge
// to add per-protocol fields here; new event types are how we extend.
package uxtelemetry

import (
	"sync"
	"time"
)

// Event is a single client-reported observation. JSON tags drive both the
// POST body deserialization and the GET response — keep them stable.
type Event struct {
	// Time is server-stamped on POST; client-supplied values are ignored
	// to keep the timeline monotonic regardless of client clock skew.
	Time time.Time `json:"time"`
	// Domain is the destination the event is about, e.g. "claude.ai" or
	// "github.com". Lowercased on Add.
	Domain string `json:"domain"`
	// EventType is the kind of measurement. Initial set:
	//   "ttfb"            — milliseconds from connect to first byte
	//   "page_load"       — full page navigation timing
	//   "ws_disconnect"   — WebSocket dropped (duration_ms = uptime before drop)
	//   "stream_stall"    — chat/SSE stream paused > 1s mid-message
	//   "request_error"   — fetch() / connect failure (duration_ms = time-to-fail)
	// Add new values via the API; the store doesn't validate.
	EventType string `json:"event_type"`
	// DurationMs is the value being reported. Semantics depend on EventType.
	DurationMs int `json:"duration_ms"`
	// ClientID is an opaque per-employee identifier (UUID generated client-side
	// once and reused). Lets summary endpoints separate "one cranky user" from
	// "everyone is hurting". Optional.
	ClientID string `json:"client_id,omitempty"`
	// Detail is a free-form note from the client (URL fragment, error code,
	// node tag observed in network tab, etc.). Bounded to 256 bytes on Add.
	Detail string `json:"detail,omitempty"`
}

// Store is a fixed-capacity in-memory ring buffer of events. Reads return
// a copy of the relevant slice so callers can't mutate the live store.
type Store struct {
	mu    sync.RWMutex
	cap   int
	buf   []Event
	head  int  // next write index
	full  bool // wrapped around at least once
}

// New creates a Store holding the most recent capacity events. capacity
// must be > 0; values < 100 are clamped up because event volume even
// from a few employees easily exceeds tiny buffers.
func New(capacity int) *Store {
	if capacity < 100 {
		capacity = 100
	}
	return &Store{cap: capacity, buf: make([]Event, capacity)}
}

// Add records an event. Server-stamps Time, lowercases Domain, truncates
// Detail. Drops the oldest event when the ring is full.
func (s *Store) Add(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.Time = time.Now().UTC()
	e.Domain = lower(e.Domain)
	if len(e.Detail) > 256 {
		e.Detail = e.Detail[:256]
	}
	s.buf[s.head] = e
	s.head = (s.head + 1) % s.cap
	if s.head == 0 {
		s.full = true
	}
}

// Snapshot returns up to limit most-recent events whose Time is at or
// after since. limit==0 means "all". since==zero means "from the start".
// Order: newest first.
func (s *Store) Snapshot(limit int, since time.Time) []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Event, 0, s.cap)
	// Walk from most-recent to oldest; ring buffer indices.
	end := s.head
	count := s.cap
	if !s.full {
		count = s.head
		end = s.head
	}
	for i := 0; i < count; i++ {
		idx := (end - 1 - i + s.cap) % s.cap
		ev := s.buf[idx]
		if !since.IsZero() && ev.Time.Before(since) {
			break
		}
		out = append(out, ev)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// Summary aggregates events from the last `window` into per-domain stats.
// Useful for /api/ux-telemetry/summary which doesn't need raw timeline,
// just "how is each domain looking right now".
type Summary struct {
	Domain     string  `json:"domain"`
	EventCount int     `json:"event_count"`
	TtfbP50    int     `json:"ttfb_p50_ms,omitempty"`
	TtfbP95    int     `json:"ttfb_p95_ms,omitempty"`
	ErrorRate  float64 `json:"error_rate"` // request_error / total
}

// Aggregate produces summaries grouped by domain for events newer than
// window. Returns sorted by EventCount desc.
func (s *Store) Aggregate(window time.Duration) []Summary {
	since := time.Now().Add(-window)
	events := s.Snapshot(0, since)

	type bucket struct {
		count  int
		errors int
		ttfbs  []int
	}
	by := map[string]*bucket{}
	for _, e := range events {
		b, ok := by[e.Domain]
		if !ok {
			b = &bucket{}
			by[e.Domain] = b
		}
		b.count++
		switch e.EventType {
		case "ttfb":
			if e.DurationMs > 0 {
				b.ttfbs = append(b.ttfbs, e.DurationMs)
			}
		case "request_error":
			b.errors++
		}
	}
	out := make([]Summary, 0, len(by))
	for d, b := range by {
		s := Summary{Domain: d, EventCount: b.count}
		if b.count > 0 {
			s.ErrorRate = float64(b.errors) / float64(b.count)
		}
		if len(b.ttfbs) > 0 {
			s.TtfbP50 = percentile(b.ttfbs, 50)
			s.TtfbP95 = percentile(b.ttfbs, 95)
		}
		out = append(out, s)
	}
	// Sort by EventCount desc — the noisier domains first.
	sortByCountDesc(out)
	return out
}

// percentile returns the linearly-interpolated p-th percentile (0 ≤ p ≤ 100).
// Slice is mutated (sorted) — caller's copy semantics handled by Aggregate
// which builds the slice fresh per bucket.
func percentile(values []int, p int) int {
	if len(values) == 0 {
		return 0
	}
	sortInts(values)
	if p <= 0 {
		return values[0]
	}
	if p >= 100 {
		return values[len(values)-1]
	}
	// Linear interpolation on rank (R-7 method).
	rank := float64(p) / 100.0 * float64(len(values)-1)
	lo := int(rank)
	hi := lo + 1
	if hi >= len(values) {
		return values[lo]
	}
	frac := rank - float64(lo)
	return int(float64(values[lo]) + frac*float64(values[hi]-values[lo]))
}

func sortInts(a []int) {
	// Tiny insertion sort is fine for typical bucket sizes (a few dozen
	// to a few hundred per domain in the window). Avoids importing sort
	// for one call, keeping the package's dependencies minimal.
	for i := 1; i < len(a); i++ {
		x := a[i]
		j := i - 1
		for j >= 0 && a[j] > x {
			a[j+1] = a[j]
			j--
		}
		a[j+1] = x
	}
}

func sortByCountDesc(s []Summary) {
	for i := 1; i < len(s); i++ {
		x := s[i]
		j := i - 1
		for j >= 0 && s[j].EventCount < x.EventCount {
			s[j+1] = s[j]
			j--
		}
		s[j+1] = x
	}
}

func lower(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}
