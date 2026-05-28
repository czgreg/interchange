// Package watchdog runs an in-process active health check on the urltest
// outbound's currently selected member. sing-box urltest only re-tests the
// pool every Interval (default 3m); a single hung node within that window
// would silently break overseas traffic.
//
// The watchdog supplements urltest's coarse periodic check with three layers:
//
//  1. Real-traffic short-circuit: if /connections shows positive byte deltas
//     on connections going through "out", skip the synthetic probe — real
//     user traffic IS the health signal. Reduces probe rate ~10x at busy
//     hours, makes us less detectable from the airport's POV.
//
//  2. Adaptive backoff + jitter: probe interval starts at cfg.Interval, doubles
//     after each healthy probe up to BackoffMax, resets to Interval on any
//     failure or selection change. Each interval is jittered ±JitterPercent so
//     the pattern isn't "exactly every 5s".
//
//  3. Primary/backup pool failover: when the rendered config has both
//     urltest-primary and urltest-backup (≥2 enabled subs), N consecutive
//     failures of primary's selected node flip the "out" selector to backup
//     instead of forcing a re-pick within primary. A separate slow goroutine
//     periodically probes primary's still-selected node; K consecutive
//     successes flip back. This guards against a whole subscription's region
//     getting blackholed without taking down overseas traffic.
package watchdog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

const (
	selectorTag       = "out"
	primaryTag        = "urltest-primary"
	backupTag         = "urltest-backup"
	connectionsPoll   = "/connections"
	httpProbeMinSlack = 2 * time.Second
)

// Watchdog polls clash-api on a (jittered, backoff-aware) interval to verify
// the active urltest member is healthy, and triggers either a forced re-pick
// (single-pool case) or a primary→backup selector flip (dual-pool case) when
// not.
type Watchdog struct {
	api      config.ClashAPIConfig
	cfg      config.WatchdogConfig
	probeURL string
	client   *http.Client

	// Random source guarded by mu — math/rand.Rand is not goroutine-safe but
	// access only happens inside Run + recovery loop, both of which take mu
	// before reading. Seeded per-Watchdog so two Watchdog instances in a
	// single test process don't share the global default source.
	rng *rand.Rand

	mu                sync.RWMutex
	lastProbe         time.Time
	consecFail        int
	consecHealthy     int
	currentNode       string // currently-selected member of the active urltest
	currentSelector   string // urltest-primary or urltest-backup
	onBackup          bool
	skippedDueTraffic int
	primaryRecovHits  int
	prevConnRX        map[string]int64 // connection.id → upload+download last seen
}

// Snapshot is a point-in-time view of the watchdog's internal state.
type Snapshot struct {
	Enabled                  bool          `json:"enabled"`
	Interval                 time.Duration `json:"-"`
	IntervalSeconds          int           `json:"interval_seconds"`
	FailThreshold            int           `json:"fail_threshold"`
	ConsecutiveFails         int           `json:"consecutive_fails"`
	ConsecutiveHealthy       int           `json:"consecutive_healthy"`
	LastProbe                time.Time     `json:"last_probe"`
	CurrentNode              string        `json:"current_node"`
	CurrentSelector          string        `json:"current_selector"`
	OnBackup                 bool          `json:"on_backup"`
	SkippedDueTraffic        int           `json:"skipped_due_traffic"`
	PrimaryRecoveryStreak    int           `json:"primary_recovery_streak"`
	PrimaryRecoveryThreshold int           `json:"primary_recovery_threshold"`
}

func New(api config.ClashAPIConfig, cfg config.WatchdogConfig, probeURL string) *Watchdog {
	httpTimeout := cfg.Timeout + httpProbeMinSlack
	if httpTimeout < 5*time.Second {
		httpTimeout = 5 * time.Second
	}
	return &Watchdog{
		api:        api,
		cfg:        cfg,
		probeURL:   probeURL,
		client:     &http.Client{Timeout: httpTimeout},
		rng:        rand.New(rand.NewSource(time.Now().UnixNano())),
		prevConnRX: make(map[string]int64),
	}
}

// Run blocks until ctx is cancelled. State transitions and forced re-picks are
// logged through slog.
func (w *Watchdog) Run(ctx context.Context) {
	if !w.cfg.Enabled {
		slog.Info("watchdog disabled by config")
		return
	}
	slog.Info("watchdog started",
		"interval", w.cfg.Interval,
		"timeout", w.cfg.Timeout,
		"fail_threshold", w.cfg.FailThreshold,
		"jitter_percent", w.cfg.JitterPercent,
		"backoff_max", w.cfg.BackoffMax,
		"real_traffic_skip", boolDeref(w.cfg.RealTrafficSkip, true),
		"probe_url", w.probeURL)

	// Recovery probe runs independently — fires only when on backup.
	go w.runPrimaryRecovery(ctx)

	currentSleep := w.cfg.Interval
	for {
		if !sleepCtx(ctx, w.jitter(currentSleep)) {
			return
		}

		// Selector "out" tells us which urltest pool is currently active.
		sel, err := w.readSelector(ctx)
		if err != nil {
			slog.Debug("watchdog: cannot read selector", "err", err)
			currentSleep = w.cfg.Interval
			continue
		}
		w.mu.Lock()
		w.currentSelector = sel
		w.onBackup = sel == backupTag
		w.mu.Unlock()

		// urltest pool member that's actually carrying traffic right now.
		now, err := w.readUrltestMember(ctx, sel)
		if err != nil || now == "" {
			slog.Debug("watchdog: cannot read urltest selection",
				"selector", sel, "err", err)
			currentSleep = w.cfg.Interval
			continue
		}
		w.mu.Lock()
		w.lastProbe = time.Now()
		prev := w.currentNode
		w.currentNode = now
		w.mu.Unlock()
		if now != prev {
			slog.Info("watchdog: selection changed",
				"selector", sel, "from", prev, "to", now)
			w.resetCounters()
			currentSleep = w.cfg.Interval
			continue
		}

		// Real-traffic short-circuit. Avoid synthetic probe when actual user
		// traffic is moving through "out" — the data plane has already proved
		// the node is alive.
		if boolDeref(w.cfg.RealTrafficSkip, true) {
			busy, derr := w.outboundBusy(ctx)
			if derr == nil && busy {
				w.mu.Lock()
				w.skippedDueTraffic++
				w.consecHealthy++
				healthy := w.consecHealthy
				w.mu.Unlock()
				slog.Debug("watchdog: skipping probe — live traffic on out",
					"node", now, "consec_healthy", healthy)
				currentSleep = w.advanceBackoff(currentSleep)
				continue
			}
		}

		// Synthetic delay probe.
		if err := w.probe(ctx, now); err != nil {
			w.mu.Lock()
			w.consecFail++
			w.consecHealthy = 0
			fails := w.consecFail
			w.mu.Unlock()
			slog.Warn("watchdog: probe failed",
				"node", now, "selector", sel, "consecutive_fails", fails, "err", err)
			currentSleep = w.cfg.Interval

			if fails < w.cfg.FailThreshold {
				continue
			}

			// Threshold hit. Behavior depends on whether a backup pool exists.
			haveBackup, berr := w.backupExists(ctx)
			if berr != nil {
				slog.Debug("watchdog: cannot probe for backup pool", "err", berr)
			}
			if haveBackup && sel == primaryTag {
				slog.Warn("watchdog: primary fail threshold reached — flipping to backup",
					"node", now, "after_fails", fails)
				if err := w.setSelector(ctx, backupTag); err != nil {
					slog.Error("watchdog: flip to backup failed", "err", err)
					continue
				}
				w.resetCounters()
				w.mu.Lock()
				w.currentSelector = backupTag
				w.onBackup = true
				w.currentNode = "" // re-read on next tick
				w.primaryRecovHits = 0
				w.mu.Unlock()
				continue
			}

			// Single-pool, OR already on backup, OR backup also bad — kick the
			// active urltest into a full re-pick.
			slog.Warn("watchdog: triggering forced re-evaluation",
				"selector", sel, "node", now, "after_fails", fails)
			if err := w.forceReselect(ctx, sel); err != nil {
				slog.Error("watchdog: force re-evaluation failed", "err", err)
				// Keep counter; retry next tick.
				continue
			}
			w.mu.Lock()
			w.consecFail = 0
			w.currentNode = "" // make next tick re-read
			w.mu.Unlock()
			continue
		}

		// Healthy probe.
		w.mu.Lock()
		recovering := w.consecFail > 0
		w.consecFail = 0
		w.consecHealthy++
		w.mu.Unlock()
		if recovering {
			slog.Info("watchdog: probe recovered", "node", now)
		}
		currentSleep = w.advanceBackoff(currentSleep)
	}
}

// advanceBackoff doubles the current sleep up to BackoffMax. BackoffMax==0
// disables backoff (always cfg.Interval).
func (w *Watchdog) advanceBackoff(curr time.Duration) time.Duration {
	if w.cfg.BackoffMax <= 0 {
		return w.cfg.Interval
	}
	next := curr * 2
	if next > w.cfg.BackoffMax {
		next = w.cfg.BackoffMax
	}
	if next < w.cfg.Interval {
		next = w.cfg.Interval
	}
	return next
}

// jitter returns d ± JitterPercent%. Bounded below by Interval/2 to avoid
// pathological tight loops if someone misconfigures jitter to ~100.
func (w *Watchdog) jitter(d time.Duration) time.Duration {
	jp := w.cfg.JitterPercent
	if jp <= 0 {
		return d
	}
	if jp > 50 {
		jp = 50
	}
	w.mu.Lock()
	frac := (w.rng.Float64()*2 - 1) * float64(jp) / 100.0
	w.mu.Unlock()
	out := time.Duration(float64(d) * (1 + frac))
	if floor := w.cfg.Interval / 2; out < floor {
		out = floor
	}
	return out
}

func (w *Watchdog) resetCounters() {
	w.mu.Lock()
	w.consecFail = 0
	w.consecHealthy = 0
	w.mu.Unlock()
}

// runPrimaryRecovery polls the primary urltest's currently-selected node on a
// long interval whenever the watchdog has flipped to backup. K consecutive
// healthy probes flip "out" back to urltest-primary. The slow cadence (default
// 30m) means a sticky outage on primary doesn't keep oscillating us back.
func (w *Watchdog) runPrimaryRecovery(ctx context.Context) {
	if w.cfg.PrimaryRecoveryInterval <= 0 || w.cfg.PrimaryRecoveryThreshold <= 0 {
		return
	}
	t := time.NewTicker(w.cfg.PrimaryRecoveryInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		w.mu.RLock()
		on := w.onBackup
		w.mu.RUnlock()
		if !on {
			// Reset streak so a future flip starts fresh.
			w.mu.Lock()
			w.primaryRecovHits = 0
			w.mu.Unlock()
			continue
		}

		// Read primary's currently-picked member directly (selector is on
		// backup so /proxies/out won't tell us).
		node, err := w.readUrltestMember(ctx, primaryTag)
		if err != nil || node == "" {
			slog.Debug("watchdog: recovery — cannot read primary member", "err", err)
			continue
		}
		if err := w.probe(ctx, node); err != nil {
			w.mu.Lock()
			w.primaryRecovHits = 0
			w.mu.Unlock()
			slog.Info("watchdog: recovery probe failed", "node", node, "err", err)
			continue
		}
		w.mu.Lock()
		w.primaryRecovHits++
		hits := w.primaryRecovHits
		need := w.cfg.PrimaryRecoveryThreshold
		w.mu.Unlock()
		slog.Info("watchdog: recovery probe OK",
			"node", node, "consecutive", hits, "needed", need)
		if hits < need {
			continue
		}

		// Threshold hit — flip back to primary.
		if err := w.setSelector(ctx, primaryTag); err != nil {
			slog.Error("watchdog: recovery flip-back failed", "err", err)
			continue
		}
		slog.Warn("watchdog: recovered — flipped back to primary",
			"primary_node", node)
		w.resetCounters()
		w.mu.Lock()
		w.currentSelector = primaryTag
		w.onBackup = false
		w.currentNode = ""
		w.primaryRecovHits = 0
		w.mu.Unlock()
	}
}

// Snapshot returns the watchdog's current internal state for API display.
// Safe to call concurrently with Run.
func (w *Watchdog) Snapshot() Snapshot {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return Snapshot{
		Enabled:                  w.cfg.Enabled,
		Interval:                 w.cfg.Interval,
		IntervalSeconds:          int(w.cfg.Interval / time.Second),
		FailThreshold:            w.cfg.FailThreshold,
		ConsecutiveFails:         w.consecFail,
		ConsecutiveHealthy:       w.consecHealthy,
		LastProbe:                w.lastProbe,
		CurrentNode:              w.currentNode,
		CurrentSelector:          w.currentSelector,
		OnBackup:                 w.onBackup,
		SkippedDueTraffic:        w.skippedDueTraffic,
		PrimaryRecoveryStreak:    w.primaryRecovHits,
		PrimaryRecoveryThreshold: w.cfg.PrimaryRecoveryThreshold,
	}
}

// readSelector returns the currently-selected member of the "out" selector,
// i.e. urltest-primary or urltest-backup (or whatever else has been pinned via
// PUT /proxies/out).
func (w *Watchdog) readSelector(ctx context.Context) (string, error) {
	var resp struct {
		Now string `json:"now"`
	}
	if err := w.get(ctx, "/proxies/"+selectorTag, &resp); err != nil {
		return "", err
	}
	if resp.Now == "" {
		return primaryTag, nil // sane default for the empty-pool bootstrap render
	}
	return resp.Now, nil
}

// readUrltestMember returns urltest's currently-selected member tag for the
// named urltest outbound. Empty on bootstrap (pool not yet ready).
func (w *Watchdog) readUrltestMember(ctx context.Context, urltestName string) (string, error) {
	var resp struct {
		Now string `json:"now"`
	}
	if err := w.get(ctx, "/proxies/"+url.PathEscape(urltestName), &resp); err != nil {
		return "", err
	}
	return resp.Now, nil
}

// backupExists checks whether urltest-backup is registered with clash-api.
// 2xx → exists; 404 → not in current config.
func (w *Watchdog) backupExists(ctx context.Context) (bool, error) {
	u := "http://" + w.api.ExternalController + "/proxies/" + url.PathEscape(backupTag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	if w.api.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+w.api.Secret)
	}
	res, err := w.client.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	if res.StatusCode == 404 {
		return false, nil
	}
	if res.StatusCode/100 != 2 {
		return false, fmt.Errorf("clash-api %s: status %d", backupTag, res.StatusCode)
	}
	return true, nil
}

// setSelector pins the "out" selector to the named member via PUT /proxies/out.
func (w *Watchdog) setSelector(ctx context.Context, member string) error {
	u := "http://" + w.api.ExternalController + "/proxies/" + selectorTag
	body := []byte(`{"name":"` + member + `"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if w.api.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+w.api.Secret)
	}
	res, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("clash-api PUT /proxies/%s: status %d", selectorTag, res.StatusCode)
	}
	return nil
}

// outboundBusy snapshots /connections, diffs upload+download against the
// previous snapshot, and returns true if any connection routed through "out"
// has a positive byte delta. Updates prev state in-place. First call after
// startup always returns false (no diff possible yet).
func (w *Watchdog) outboundBusy(ctx context.Context) (bool, error) {
	var resp struct {
		Connections []struct {
			ID       string   `json:"id"`
			Upload   int64    `json:"upload"`
			Download int64    `json:"download"`
			Chains   []string `json:"chains"`
		} `json:"connections"`
	}
	if err := w.get(ctx, connectionsPoll, &resp); err != nil {
		return false, err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	curr := make(map[string]int64, len(resp.Connections))
	busy := false
	for _, c := range resp.Connections {
		if !chainsContainOut(c.Chains) {
			continue
		}
		total := c.Upload + c.Download
		curr[c.ID] = total
		prev, ok := w.prevConnRX[c.ID]
		if ok && total > prev {
			busy = true
		}
	}
	// Replace map so dead connection IDs don't accumulate.
	w.prevConnRX = curr
	return busy, nil
}

// chainsContainOut accepts the clash-api chains array (leaf-to-root order:
// e.g. ["yuyun/SG01", "urltest-primary", "out"]) and reports whether "out"
// appears anywhere — meaning the connection traversed our selector.
func chainsContainOut(chains []string) bool {
	for _, c := range chains {
		if c == selectorTag {
			return true
		}
	}
	return false
}

// probe triggers a single delay measurement on the named node. clash-api
// returns 200 with {"delay": N} on success, or non-2xx / a non-empty
// "message" on failure.
func (w *Watchdog) probe(ctx context.Context, node string) error {
	timeoutMs := int(w.cfg.Timeout / time.Millisecond)
	p := fmt.Sprintf("/proxies/%s/delay?url=%s&timeout=%d",
		url.PathEscape(node), url.QueryEscape(w.probeURL), timeoutMs)
	var resp struct {
		Delay   int    `json:"delay"`
		Message string `json:"message"`
	}
	if err := w.get(ctx, p, &resp); err != nil {
		return err
	}
	if resp.Message != "" {
		return fmt.Errorf("delay test reported: %s", resp.Message)
	}
	if resp.Delay <= 0 {
		return fmt.Errorf("delay = %d (no measurement)", resp.Delay)
	}
	return nil
}

// forceReselect makes the named urltest re-test every member in parallel and
// re-pick the lowest-RTT survivor.
func (w *Watchdog) forceReselect(ctx context.Context, urltestName string) error {
	timeoutMs := int(5 * time.Second / time.Millisecond)
	p := fmt.Sprintf("/proxies/%s/delay?url=%s&timeout=%d",
		url.PathEscape(urltestName), url.QueryEscape(w.probeURL), timeoutMs)
	// Full-pool tests can take 10s; need a longer-lived client.
	client := &http.Client{Timeout: 30 * time.Second}
	u := "http://" + w.api.ExternalController + p
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if w.api.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+w.api.Secret)
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("clash-api %s/delay: status %d", urltestName, res.StatusCode)
	}
	return nil
}

func (w *Watchdog) get(ctx context.Context, path string, out any) error {
	u := "http://" + w.api.ExternalController + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if w.api.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+w.api.Secret)
	}
	res, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("clash-api %s: status %d", path, res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func boolDeref(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// sleepCtx waits d or until ctx is done. Returns false if ctx cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
