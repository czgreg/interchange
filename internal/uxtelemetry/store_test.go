package uxtelemetry

import (
	"testing"
	"time"
)

func TestStore_AddAndSnapshot(t *testing.T) {
	s := New(5)
	s.Add(Event{Domain: "claude.ai", EventType: "ttfb", DurationMs: 250})
	s.Add(Event{Domain: "GitHub.com", EventType: "ttfb", DurationMs: 180})
	s.Add(Event{Domain: "claude.ai", EventType: "request_error", DurationMs: 5000})

	out := s.Snapshot(0, time.Time{})
	if len(out) != 3 {
		t.Fatalf("want 3 events, got %d", len(out))
	}
	// Newest first — request_error came last.
	if out[0].EventType != "request_error" {
		t.Errorf("oldest-first ordering broken: out[0]=%v", out[0])
	}
	// Domain lowercased on Add.
	for _, e := range out {
		if e.Domain == "GitHub.com" {
			t.Error("domain not lowercased")
		}
	}
	// Time was server-stamped.
	for _, e := range out {
		if e.Time.IsZero() {
			t.Error("Time not stamped on Add")
		}
	}
}

func TestStore_RingOverflow(t *testing.T) {
	s := New(100) // capacity floored to 100
	for i := 0; i < 250; i++ {
		s.Add(Event{Domain: "x.com", EventType: "ttfb", DurationMs: i})
	}
	out := s.Snapshot(0, time.Time{})
	if len(out) != 100 {
		t.Fatalf("want 100 (cap), got %d", len(out))
	}
	// Last event was DurationMs=249 (newest); first in snapshot should be that.
	if out[0].DurationMs != 249 {
		t.Errorf("newest first broken: out[0].duration=%d, want 249", out[0].DurationMs)
	}
	// Last in snapshot should be DurationMs=150 (oldest still in ring).
	if out[99].DurationMs != 150 {
		t.Errorf("ring eviction broken: out[99].duration=%d, want 150 (250-100)", out[99].DurationMs)
	}
}

func TestStore_Aggregate(t *testing.T) {
	s := New(100)
	// 10 ttfb events on claude.ai with linearly-spaced durations.
	for i := 1; i <= 10; i++ {
		s.Add(Event{Domain: "claude.ai", EventType: "ttfb", DurationMs: i * 100})
	}
	// 1 request_error mixed in.
	s.Add(Event{Domain: "claude.ai", EventType: "request_error"})
	// Some events on github.com.
	s.Add(Event{Domain: "github.com", EventType: "ttfb", DurationMs: 50})

	summary := s.Aggregate(1 * time.Hour)
	if len(summary) != 2 {
		t.Fatalf("want 2 domains, got %d", len(summary))
	}
	// Sorted by count desc → claude.ai (11 events) first.
	if summary[0].Domain != "claude.ai" {
		t.Errorf("sort order: summary[0].domain=%s", summary[0].Domain)
	}
	if summary[0].EventCount != 11 {
		t.Errorf("claude.ai count = %d, want 11", summary[0].EventCount)
	}
	// p50 of 100..1000 in 100 steps = 550 (linear interp between 500 and 600)
	if summary[0].TtfbP50 < 500 || summary[0].TtfbP50 > 600 {
		t.Errorf("ttfb p50 = %d, expected 500-600", summary[0].TtfbP50)
	}
	// 1 error / 11 events ≈ 0.0909
	if summary[0].ErrorRate < 0.08 || summary[0].ErrorRate > 0.10 {
		t.Errorf("error_rate = %v, want ~0.0909", summary[0].ErrorRate)
	}
}

func TestStore_SinceFilter(t *testing.T) {
	s := New(100)
	s.Add(Event{Domain: "old.com", EventType: "ttfb", DurationMs: 1})
	time.Sleep(20 * time.Millisecond)
	threshold := time.Now()
	time.Sleep(5 * time.Millisecond)
	s.Add(Event{Domain: "new.com", EventType: "ttfb", DurationMs: 2})

	out := s.Snapshot(0, threshold)
	if len(out) != 1 || out[0].Domain != "new.com" {
		t.Errorf("since filter broken: %+v", out)
	}
}
