// passive.go — connection-lifecycle stats from mihomo's /connections.
//
// Pure localhost observation: every passivePollInterval we snapshot
// /connections, diff the live connection-ID set against the previous
// snapshot, and for each connection that disappeared (closed) classify it
// as "failed" if it moved fewer than minHandshakeBytes before closing —
// the signature of an upstream RST / dead egress / captive redirect that
// RTT probes don't catch. Per-node close events accumulate in a rolling
// window; FailRate = failed/closed over that window.
//
// Zero airport traffic — this only reads mihomo's local clash-api.

package nodescorer

import (
	"context"
	"time"
)

const (
	// passivePollInterval is how often /connections is snapshotted. Fine
	// enough to catch short-lived failed connections that a 60s scoring
	// tick would miss entirely.
	passivePollInterval = 10 * time.Second
	// passiveWindow is the rolling window over which close events are
	// retained for the fail-rate calculation.
	passiveWindow = 10 * time.Minute
	// minHandshakeBytes: a connection that closed having moved fewer than
	// this many bytes (up+down) never completed a useful exchange — even a
	// bare TLS 1.3 handshake is ~5KB. Treated as a failed connection.
	minHandshakeBytes = 2048
)

func (s *Scorer) runPassiveLoop(ctx context.Context) {
	tick := time.NewTicker(passivePollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.passivePoll(ctx)
		}
	}
}

// passivePoll snapshots /connections and updates the per-node close-event
// log + live-conn counts.
func (s *Scorer) passivePoll(ctx context.Context) {
	conns, err := s.fetchConnections(ctx)
	if err != nil {
		return
	}
	now := time.Now()

	// Build the current live set: connID → {node, bytes}.
	curr := make(map[string]connInfo, len(conns))
	active := map[string]int{}
	for _, c := range conns {
		id, _ := c["id"].(string)
		if id == "" {
			continue
		}
		chains, _ := c["chains"].([]interface{})
		if len(chains) == 0 {
			continue
		}
		node, _ := chains[0].(string)
		if node == "" {
			continue
		}
		up, _ := c["upload"].(float64)
		dn, _ := c["download"].(float64)
		curr[id] = connInfo{node: node, bytes: int64(up + dn)}
		active[node]++
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Detect closed connections: present last poll, gone now.
	for id, prev := range s.connSeen {
		if _, stillLive := curr[id]; stillLive {
			continue
		}
		ev := closeEvent{at: now, failed: prev.bytes < minHandshakeBytes}
		s.closeEvents[prev.node] = append(s.closeEvents[prev.node], ev)
	}

	s.connSeen = curr
	s.activeConns = active

	// Prune close events older than the window.
	cutoff := now.Add(-passiveWindow)
	for node, evs := range s.closeEvents {
		keep := evs[:0]
		for _, e := range evs {
			if e.at.After(cutoff) {
				keep = append(keep, e)
			}
		}
		if len(keep) == 0 {
			delete(s.closeEvents, node)
		} else {
			s.closeEvents[node] = keep
		}
	}
}

// passiveStatsLocked builds the PassiveStats for one node. CALLER MUST
// HOLD s.mu. Returns nil when there's nothing observed yet (no active
// conns and no recent closes) so the JSON field stays absent.
func (s *Scorer) passiveStatsLocked(node string) *PassiveStats {
	active := s.activeConns[node]
	evs := s.closeEvents[node]
	if active == 0 && len(evs) == 0 {
		return nil
	}
	failed := 0
	for _, e := range evs {
		if e.failed {
			failed++
		}
	}
	var rate float64
	if len(evs) > 0 {
		rate = float64(failed) / float64(len(evs))
	}
	return &PassiveStats{
		ActiveConns:     active,
		ClosedWindow:    len(evs),
		FailedWindow:    failed,
		FailRate:        rate,
		SampleWindowSec: int(passiveWindow.Seconds()),
	}
}
