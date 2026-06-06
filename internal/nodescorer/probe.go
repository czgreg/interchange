// probe.go — site-specific reachability probing.
//
// The probe loop runs in its own goroutine (started by Run when a pool
// gates on probes). Each sweep:
//
//  1. Reads the current candidate node list from the latest snapshot.
//  2. For each node with at least one due probe, flips the probe-out
//     selector (clash-api PUT /proxies/probe-out {name: <node>}) so the
//     leap-probe listener (127.0.0.1:11081) egresses via that node.
//  3. Issues each due probe's GET through the leap-probe proxy, recording
//     status code, cf-mitigated header, latency, and error.
//
// Probing is fully serialized inside this one goroutine — the shared
// probe-out selector is only ever flipped here, so no locking around the
// selector is needed. Results land in s.probeResults under s.mu; score()
// reads them to gate named-pool membership.
//
// Traffic cost is deliberately modest: each (node, probe) runs at most once
// per probe.Interval (default 5m). See docs/api.md capacity notes.

package nodescorer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

// probeOutSelectorName is the clash-api selector group nodescorer flips to
// point the leap-probe listener at one node at a time. Must match the name
// the mihomo renderer emits (internal/mihomo/groups.go probeOutSelector).
const probeOutSelectorName = "probe-out"

// probeSweepInterval is how often the loop wakes to check for due probes.
// Finer than any probe Interval so a probe due "every 5m" fires within
// ~30s of its deadline rather than up to a full sweep late.
const probeSweepInterval = 30 * time.Second

func (s *Scorer) runProbeLoop(ctx context.Context) {
	slog.Info("nodescorer: probe loop started", "probes", len(s.cfg.Probes))
	// Small initial delay so the first config render + mihomo reload settle
	// and probe-out has members.
	t := time.NewTimer(15 * time.Second)
	select {
	case <-ctx.Done():
		t.Stop()
		return
	case <-t.C:
	}
	s.probeSweep(ctx)

	tick := time.NewTicker(probeSweepInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.probeSweep(ctx)
		}
	}
}

// probeSweep runs every due (node, probe) pair. Groups by node so the
// probe-out selector is flipped once per node, then all that node's due
// probes run back-to-back.
func (s *Scorer) probeSweep(ctx context.Context) {
	candidates := s.probeCandidates()
	if len(candidates) == 0 {
		return
	}
	now := time.Now()
	for _, tag := range candidates {
		if ctx.Err() != nil {
			return
		}
		due := s.dueProbes(tag, now)
		if len(due) == 0 {
			continue
		}
		// Flip probe-out → this node. On failure, skip the node this sweep.
		if err := s.setSelector(ctx, probeOutSelectorName, tag); err != nil {
			slog.Warn("nodescorer: probe selector flip failed", "node", tag, "err", err)
			continue
		}
		// Brief settle so mihomo applies the selector before we dial.
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
		for _, p := range due {
			res := s.runProbe(ctx, p)
			s.storeProbeResult(tag, p.Name, res, now)
		}
	}
}

// probeCandidates returns the node tags worth probing: the union of the
// current us-pool qualified set and any node already in the snapshot. We
// probe even non-qualified nodes so a node that recovers RTT-wise AND
// passes its site probe can be admitted to a pool without delay.
func (s *Scorer) probeCandidates() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for tag := range s.poolSet {
		if !seen[tag] {
			seen[tag] = true
			out = append(out, tag)
		}
	}
	for _, n := range s.snap.Nodes {
		if n.Alive && !seen[n.Name] {
			seen[n.Name] = true
			out = append(out, n.Name)
		}
	}
	sort.Strings(out)
	return out
}

// dueProbes returns the probe configs for tag whose Interval has elapsed
// since their last run (or that have never run).
func (s *Scorer) dueProbes(tag string, now time.Time) []config.ProbeConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	last := s.probeLast[tag]
	var due []config.ProbeConfig
	for _, p := range s.cfg.Probes {
		iv := p.Interval
		if iv <= 0 {
			iv = 5 * time.Minute
		}
		if last == nil || now.Sub(last[p.Name]) >= iv {
			due = append(due, p)
		}
	}
	return due
}

// runProbe issues one GET through the leap-probe proxy and classifies the
// result. The proxy egresses via whatever node probe-out currently points
// at (set by the caller). latency on failure = time waited before the
// error, not 0.
func (s *Scorer) runProbe(ctx context.Context, p config.ProbeConfig) ProbeResult {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	res := ProbeResult{LastCheckedAt: start.UTC()}

	req, err := http.NewRequestWithContext(rctx, http.MethodGet, p.URL, nil)
	if err != nil {
		res.LastError = err.Error()
		res.LatencyMs = int(time.Since(start).Milliseconds())
		return res
	}
	// Look like a real browser — airports / CF fingerprint odd UAs.
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36")

	resp, err := s.probeHTTPc.Do(req)
	res.LatencyMs = int(time.Since(start).Milliseconds())
	if err != nil {
		res.LastError = err.Error()
		return res
	}
	defer resp.Body.Close()
	res.StatusCode = resp.StatusCode

	// Cloudflare bot-challenge detection: CF returns 200 + a JS-challenge
	// HTML body but sets cf-mitigated: challenge. Treat as failure even
	// though the status code is in range.
	if cf := resp.Header.Get("cf-mitigated"); strings.EqualFold(cf, "challenge") {
		res.CFMitigated = true
		res.LastError = "cloudflare challenge (cf-mitigated: challenge)"
		return res
	}

	maxStatus := p.Check.MaxStatus
	if maxStatus == 0 {
		maxStatus = 399
	}
	if resp.StatusCode <= maxStatus && resp.StatusCode >= 100 {
		res.OK = true
	} else {
		res.LastError = "status " + http.StatusText(resp.StatusCode)
	}
	return res
}

func (s *Scorer) storeProbeResult(tag, name string, res ProbeResult, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.probeResults[tag] == nil {
		s.probeResults[tag] = map[string]ProbeResult{}
	}
	s.probeResults[tag][name] = res
	if s.probeLast[tag] == nil {
		s.probeLast[tag] = map[string]time.Time{}
	}
	s.probeLast[tag][name] = now
}

// setSelector PUTs clash-api /proxies/<selector> {name: <member>} to point
// a select group at a specific member. Used to aim probe-out at one node.
func (s *Scorer) setSelector(ctx context.Context, selector, member string) error {
	body, _ := json.Marshal(map[string]string{"name": member})
	u := "http://" + s.apiAddr + "/proxies/" + url.PathEscape(selector)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, jsonBodyReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.apiSecret != "" {
		req.Header.Set("Authorization", "Bearer "+s.apiSecret)
	}
	resp, err := s.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("PUT /proxies/%s: HTTP %d", selector, resp.StatusCode)
	}
	return nil
}
