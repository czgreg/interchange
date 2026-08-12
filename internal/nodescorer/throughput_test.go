package nodescorer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// throughputScorer wires a Scorer against a stub /connections that serves
// whatever the current snapshot pointer holds. Only the fields
// updateThroughput touches are populated.
func throughputScorer(t *testing.T, snapshot *[]map[string]interface{}) *Scorer {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/connections" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"connections": *snapshot})
	}))
	t.Cleanup(srv.Close)
	return &Scorer{
		apiAddr:  srv.Listener.Addr().String(),
		httpc:    srv.Client(),
		connPrev: map[string]connBytes{},
	}
}

func conn(id, node string, up, dn int64) map[string]interface{} {
	return map[string]interface{}{
		"id":       id,
		"chains":   []interface{}{node, "us-pool"},
		"upload":   float64(up),
		"download": float64(dn),
	}
}

// The bug: connPrev was keyed by NODE, so the delta was taken between two
// sums over the LIVE connection set. When a connection closed, its bytes
// left the current sum, the node delta went negative, and the clamp
// discarded the whole node's interval — including every still-live
// connection's traffic on that node.
//
// Here c1 (large, closes) and c2 (small, keeps running) share a node. c2
// moves 1000 bytes over the interval and must be reported.
func TestUpdateThroughput_ClosedConnDoesNotZeroTheNode(t *testing.T) {
	snap := []map[string]interface{}{
		conn("c1", "sub/n1", 500_000, 500_000),
		conn("c2", "sub/n1", 500, 500),
	}
	s := throughputScorer(t, &snap)
	ctx := context.Background()

	if got := s.updateThroughput(ctx, nil); len(got) != 0 {
		t.Fatalf("first poll establishes the baseline, must report nothing; got %v", got)
	}
	// Backdate so dt is a known 10s rather than ~0.
	s.connPrevT = s.connPrevT.Add(-10 * time.Second)

	// c1 closed; c2 advanced by 1000 total.
	snap = []map[string]interface{}{conn("c2", "sub/n1", 1000, 1000)}

	got := s.updateThroughput(ctx, nil)
	// 1000 bytes / 10s = 100 bps.
	if bps := got["sub/n1"]; bps != 100 {
		t.Errorf("still-live c2 moved 1000B/10s → want 100 bps for sub/n1, got %v (full map %v)", bps, got)
	}
}

// A connection appearing mid-interval has necessarily moved all of its
// bytes during that interval, so counting the full amount is correct — but
// it must be attributed once, not re-counted on the following poll.
func TestUpdateThroughput_NewConnCountedOnceNotTwice(t *testing.T) {
	snap := []map[string]interface{}{conn("c1", "sub/n1", 0, 0)}
	s := throughputScorer(t, &snap)
	ctx := context.Background()
	s.updateThroughput(ctx, nil)
	s.connPrevT = s.connPrevT.Add(-10 * time.Second)

	// c2 appears having already moved 2000B; c1 idle.
	snap = []map[string]interface{}{
		conn("c1", "sub/n1", 0, 0),
		conn("c2", "sub/n1", 1000, 1000),
	}
	if bps := s.updateThroughput(ctx, nil)["sub/n1"]; bps != 200 {
		t.Errorf("new conn 2000B/10s → want 200 bps, got %v", bps)
	}

	// Next interval: nothing moves. The old node-keyed code would have
	// re-reported c2's cumulative bytes as fresh traffic on the first poll
	// after it appeared; assert the counter is now a true baseline.
	s.connPrevT = s.connPrevT.Add(-10 * time.Second)
	if bps := s.updateThroughput(ctx, nil)["sub/n1"]; bps != 0 {
		t.Errorf("idle interval must report no throughput, got %v", bps)
	}
}

// Deltas must be attributed to the node each connection actually egresses
// through, not merged across nodes.
func TestUpdateThroughput_AttributesPerNode(t *testing.T) {
	snap := []map[string]interface{}{
		conn("c1", "sub/n1", 0, 0),
		conn("c2", "sub/n2", 0, 0),
	}
	s := throughputScorer(t, &snap)
	ctx := context.Background()
	s.updateThroughput(ctx, nil)
	s.connPrevT = s.connPrevT.Add(-10 * time.Second)

	snap = []map[string]interface{}{
		conn("c1", "sub/n1", 1000, 0),   // +1000 → 100 bps
		conn("c2", "sub/n2", 0, 20_000), // +20000 → 2000 bps
	}
	got := s.updateThroughput(ctx, nil)
	if got["sub/n1"] != 100 || got["sub/n2"] != 2000 {
		t.Errorf("want n1=100 n2=2000, got %v", got)
	}
}

// mihomo restarts reset the counters. A lower value than last seen cannot
// be a real delta, so it must be treated as a fresh connection rather than
// producing a negative contribution that cancels sibling traffic.
func TestUpdateThroughput_CounterResetTreatedAsNew(t *testing.T) {
	snap := []map[string]interface{}{conn("c1", "sub/n1", 900_000, 900_000)}
	s := throughputScorer(t, &snap)
	ctx := context.Background()
	s.updateThroughput(ctx, nil)
	s.connPrevT = s.connPrevT.Add(-10 * time.Second)

	// Same id, counters reset to a small value.
	snap = []map[string]interface{}{conn("c1", "sub/n1", 250, 250)}
	if bps := s.updateThroughput(ctx, nil)["sub/n1"]; bps != 50 {
		t.Errorf("reset counters → count as new (500B/10s = 50 bps), got %v", bps)
	}
}
