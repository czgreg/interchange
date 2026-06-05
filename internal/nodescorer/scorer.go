// Package nodescorer implements dynamic pool management for engine=mihomo.
//
// The scorer runs a goroutine that periodically:
//  1. Reads every node's probe history from mihomo's /proxies endpoint and
//     computes RTT statistics (p50, p95, jitter, fail_rate).
//  2. Passively reads /connections to derive per-node real-traffic throughput
//     (upload+download bytes/sec from active connections) — no extra bandwidth
//     consumed.
//  3. Compares each node against the configured NodeQualify thresholds.
//  4. If the qualified set has changed, re-renders mihomo's config.yaml with
//     the updated us-pool member list and calls PUT /configs (mihomo hot-reload).
//     Existing connections drain naturally; new connections use the new config.
//
// The scorer also exposes NodeHealth state for /api/nodes/health.
package nodescorer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
	"github.com/leap-gateway/leap-gateway/internal/subscribe"
)

// NodeHealth holds the latest scored state of one proxy node.
type NodeHealth struct {
	Name string `json:"name"`
	Sub  string `json:"sub"` // subscription prefix before first "/"

	// RTT stats from mihomo probe history (most recent MinProbes..8 entries).
	Alive      bool    `json:"alive"`
	LastRTTMs  int     `json:"rtt_last_ms"`
	RTTP50Ms   int     `json:"rtt_p50_ms"`
	RTTP95Ms   int     `json:"rtt_p95_ms"`
	JitterMs   int     `json:"jitter_ms"`  // p95 - p50
	FailRate   float64 `json:"fail_rate"`  // fraction of probes with delay=0
	ProbeCount int     `json:"probe_count"` // history entries available

	// Passive throughput from /connections (bytes/sec, 0 when no active traffic).
	ThroughputBps float64 `json:"throughput_bps"`

	// Pool membership state.
	Qualified bool   `json:"qualified"`
	InPool    bool   `json:"in_pool"`
	Strikes   int    `json:"strikes"`         // consecutive failing rounds
	OkRounds  int    `json:"ok_rounds"`       // consecutive passing rounds
	Reason    string `json:"reason,omitempty"` // why not qualified (if !Qualified)
}

// Snapshot is the complete scorer state at one instant.
type Snapshot struct {
	Qualified      int         `json:"qualified"`
	Total          int         `json:"total"`
	LastPoolUpdate time.Time   `json:"last_pool_update"`
	LastScoredAt   time.Time   `json:"last_scored_at"`
	Thresholds     interface{} `json:"thresholds"`
	Nodes          []NodeHealth `json:"nodes"`
}

// Renderer is the subset of the mihomo renderer used by NodeScorer to
// rebuild config.yaml when the pool membership changes.
type Renderer interface {
	// RenderWithQualifiedNodes produces a new config YAML using only the
	// given node tags as us-pool members. It does NOT write to disk — the
	// scorer writes the bytes itself so it can diff + hot-reload.
	RenderWithQualifiedNodes(outbounds []subscribe.Outbound, qualified []string) ([]byte, error)
	// Path returns the on-disk path of the config file.
	Path() string
}

// Scorer is the dynamic pool manager.
type Scorer struct {
	cfg         config.NodeQualifyConfig
	nodePattern string // regexp filter matching sing-box renderer's NodePattern
	apiAddr     string // mihomo clash-api host:port
	apiSecret   string
	probeURL    string // same URL mihomo uses for url-test
	renderer    Renderer
	subscribe   func() []subscribe.Outbound // live outbounds from subscribe.Manager

	mu      sync.RWMutex
	state   map[string]*nodeState // keyed by node tag
	snap    Snapshot
	poolSet map[string]bool // current qualified set in config

	httpc *http.Client

	// throughput tracking: previous /connections snapshot
	connPrev map[string]connBytes
	connPrevT time.Time
}

type nodeState struct {
	health  NodeHealth
	strikes int
	okRuns  int
}

type connBytes struct {
	upload   int64
	download int64
}

// New creates a Scorer. probeURL should match mihomo's url-test URL (same
// probe target = same measurement conditions).
func New(
	cfg config.NodeQualifyConfig,
	apiAddr, apiSecret, probeURL, nodePattern string,
	r Renderer,
	allOutbounds func() []subscribe.Outbound,
) *Scorer {
	return &Scorer{
		cfg:         cfg,
		nodePattern: nodePattern,
		apiAddr:     apiAddr,
		apiSecret:   apiSecret,
		probeURL:    probeURL,
		renderer:    r,
		subscribe:   allOutbounds,
		state:       map[string]*nodeState{},
		poolSet:     map[string]bool{},
		connPrev:    map[string]connBytes{},
		httpc:       &http.Client{Timeout: 5 * time.Second},
	}
}

// Run blocks until ctx is cancelled. Each ScoringInterval it scores all
// nodes and hot-reloads mihomo when the pool changes.
func (s *Scorer) Run(ctx context.Context) {
	slog.Info("nodescorer: started",
		"interval", s.cfg.ScoringInterval,
		"max_rtt_p50", s.cfg.MaxRTTP50Ms,
		"max_rtt_p95", s.cfg.MaxRTTP95Ms,
		"max_jitter", s.cfg.MaxJitterMs,
		"max_fail_rate", s.cfg.MaxFailRate,
		"evict_strikes", s.cfg.EvictStrikes,
	)
	// Initial score after a short warm-up so probe history is populated.
	warmup := time.NewTimer(10 * time.Second)
	select {
	case <-ctx.Done():
		return
	case <-warmup.C:
	}
	s.score(ctx)

	tick := time.NewTicker(s.cfg.ScoringInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.score(ctx)
		}
	}
}

// GetSnapshot returns a copy of the latest scored state.
func (s *Scorer) GetSnapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := s.snap
	nodes := make([]NodeHealth, len(snap.Nodes))
	copy(nodes, snap.Nodes)
	snap.Nodes = nodes
	return snap
}

func (s *Scorer) score(ctx context.Context) {
	// 1. Fetch /proxies from mihomo.
	proxies, err := s.fetchProxies(ctx)
	if err != nil {
		slog.Warn("nodescorer: /proxies fetch failed", "err", err)
		return
	}

	// 2. Fetch /connections for passive throughput.
	throughput := s.updateThroughput(ctx, proxies)

	// 3. Build candidate set = union of (current us-pool members) and (nodes
	//    previously seen in state). This keeps evicted nodes under observation
	//    without needing to re-run NodePattern regexp (which has edge-case
	//    matching issues in Go's RE2 for patterns like \bUS).
	usPool := proxies["us-pool"]
	if usPool == nil {
		slog.Warn("nodescorer: us-pool not found in /proxies")
		return
	}
	members, _ := usPool["all"].([]interface{})
	candidateSet := make(map[string]bool, len(members)+len(s.state))
	for _, raw := range members {
		if tag, ok := raw.(string); ok && tag != "" {
			candidateSet[tag] = true
		}
	}
	for tag := range s.state {
		// Re-include previously scored nodes only if they still exist in /proxies
		if _, ok := proxies[tag]; ok {
			candidateSet[tag] = true
		}
	}
	candidates := make([]string, 0, len(candidateSet))
	for tag := range candidateSet {
		candidates = append(candidates, tag)
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		slog.Warn("nodescorer: no candidates (us-pool empty and no prior state)")
		return
	}

	// 4. Score each member.
	s.mu.Lock()
	defer s.mu.Unlock()

	newPoolSet := map[string]bool{}
	nodes := make([]NodeHealth, 0, len(candidates))

	for _, tag := range candidates {
		pData, ok := proxies[tag]
		if !ok {
			continue
		}
		h := s.scoreNode(tag, pData, throughput[tag])
		// Retrieve or create state.
		st, exists := s.state[tag]
		if !exists {
			st = &nodeState{}
			s.state[tag] = st
		}
		st.health = h

		// Was already in pool?
		prevInPool := s.poolSet[tag]

		if h.Qualified {
			st.strikes = 0
			st.okRuns++
			// Readmit logic: if not currently in pool, need ReadmitStrikes.
			if prevInPool || st.okRuns >= s.cfg.ReadmitStrikes {
				newPoolSet[tag] = true
				h.InPool = true
			}
		} else {
			st.okRuns = 0
			st.strikes++
			// Keep in pool until EvictStrikes exceeded.
			if prevInPool && st.strikes < s.cfg.EvictStrikes {
				newPoolSet[tag] = true
				h.InPool = true
			}
		}
		h.Strikes = st.strikes
		h.OkRounds = st.okRuns
		st.health = h
		nodes = append(nodes, h)
	}

	// Sort: qualified first, then by RTT.
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Qualified != nodes[j].Qualified {
			return nodes[i].Qualified
		}
		return nodes[i].RTTP50Ms < nodes[j].RTTP50Ms
	})

	qualified := 0
	for _, n := range nodes {
		if n.InPool {
			qualified++
		}
	}

	now := time.Now().UTC()
	poolChanged := !poolSetsEqual(s.poolSet, newPoolSet)
	lastPoolUpdate := s.snap.LastPoolUpdate
	if poolChanged {
		lastPoolUpdate = now
	}

	s.snap = Snapshot{
		Qualified:      qualified,
		Total:          len(nodes),
		LastPoolUpdate: lastPoolUpdate,
		LastScoredAt:   now,
		Thresholds:     s.cfg,
		Nodes:          nodes,
	}
	s.poolSet = newPoolSet

	slog.Info("nodescorer: scored",
		"qualified", qualified,
		"total", len(nodes),
		"pool_changed", poolChanged,
	)

	if poolChanged {
		go s.hotReload(context.Background(), newPoolSet)
	}
}

func (s *Scorer) scoreNode(tag string, pData map[string]interface{}, throughputBps float64) NodeHealth {
	h := NodeHealth{
		Name:          tag,
		ThroughputBps: throughputBps,
	}
	if idx := indexOf(tag, "/"); idx >= 0 {
		h.Sub = tag[:idx]
	}

	alive, _ := pData["alive"].(bool)
	h.Alive = alive

	// Extract probe history from extra["<probeURL>"].history
	history := s.extractHistory(pData)
	h.ProbeCount = len(history)

	if len(history) == 0 {
		if !alive {
			h.Reason = "dead"
		} else {
			h.Reason = "no probe history yet"
		}
		return h
	}

	// Compute statistics.
	var delays []int
	failed := 0
	for _, d := range history {
		if d == 0 {
			failed++
		} else {
			delays = append(delays, d)
		}
	}
	h.FailRate = float64(failed) / float64(len(history))
	if len(delays) > 0 {
		h.LastRTTMs = history[0] // most recent
		sortInts(delays)
		h.RTTP50Ms = percentile(delays, 50)
		h.RTTP95Ms = percentile(delays, 95)
		h.JitterMs = h.RTTP95Ms - h.RTTP50Ms
	}

	// Qualify check.
	q := s.cfg
	if !alive {
		h.Reason = "dead"
		return h
	}
	if h.ProbeCount < q.MinProbes {
		h.Qualified = true // not enough data → give benefit of doubt
		return h
	}
	if h.FailRate > q.MaxFailRate {
		h.Reason = fmt.Sprintf("fail_rate=%.2f > %.2f", h.FailRate, q.MaxFailRate)
		return h
	}
	if h.RTTP50Ms > q.MaxRTTP50Ms {
		h.Reason = fmt.Sprintf("rtt_p50=%dms > %dms", h.RTTP50Ms, q.MaxRTTP50Ms)
		return h
	}
	if h.RTTP95Ms > q.MaxRTTP95Ms {
		h.Reason = fmt.Sprintf("rtt_p95=%dms > %dms", h.RTTP95Ms, q.MaxRTTP95Ms)
		return h
	}
	if h.JitterMs > q.MaxJitterMs {
		h.Reason = fmt.Sprintf("jitter=%dms > %dms", h.JitterMs, q.MaxJitterMs)
		return h
	}
	h.Qualified = true
	return h
}

func (s *Scorer) extractHistory(pData map[string]interface{}) []int {
	// mihomo stores per-url history under extra["<probeURL>"].history
	extra, _ := pData["extra"].(map[string]interface{})
	if extra == nil {
		return nil
	}
	urlData, _ := extra[s.probeURL].(map[string]interface{})
	if urlData == nil {
		// fallback: try any entry in extra
		for _, v := range extra {
			urlData, _ = v.(map[string]interface{})
			break
		}
	}
	if urlData == nil {
		return nil
	}
	rawHist, _ := urlData["history"].([]interface{})
	out := make([]int, 0, len(rawHist))
	for _, entry := range rawHist {
		m, _ := entry.(map[string]interface{})
		if m == nil {
			continue
		}
		d, _ := m["delay"].(float64)
		out = append(out, int(d))
	}
	return out
}

func (s *Scorer) updateThroughput(ctx context.Context, proxies map[string]map[string]interface{}) map[string]float64 {
	conns, err := s.fetchConnections(ctx)
	if err != nil {
		return nil
	}
	now := time.Now()
	curr := map[string]connBytes{}
	// Find which node (leaf of chains) each connection goes through.
	for _, c := range conns {
		chains, _ := c["chains"].([]interface{})
		if len(chains) == 0 {
			continue
		}
		// chains is leaf→root; chains[0] is the actual proxy node.
		node, _ := chains[0].(string)
		if node == "" {
			continue
		}
		up, _ := c["upload"].(float64)
		dn, _ := c["download"].(float64)
		cb := curr[node]
		cb.upload += int64(up)
		cb.download += int64(dn)
		curr[node] = cb
	}

	result := map[string]float64{}
	dt := now.Sub(s.connPrevT).Seconds()
	if s.connPrevT.IsZero() || dt <= 0 {
		s.connPrev = curr
		s.connPrevT = now
		return result
	}
	for node, cb := range curr {
		prev := s.connPrev[node]
		deltaUp := cb.upload - prev.upload
		deltaDn := cb.download - prev.download
		if deltaUp < 0 {
			deltaUp = 0
		}
		if deltaDn < 0 {
			deltaDn = 0
		}
		bps := float64(deltaUp+deltaDn) / dt
		if bps > 0 {
			result[node] = math.Round(bps)
		}
	}
	s.connPrev = curr
	s.connPrevT = now
	return result
}

func (s *Scorer) hotReload(ctx context.Context, newPool map[string]bool) {
	qualified := make([]string, 0, len(newPool))
	for tag := range newPool {
		qualified = append(qualified, tag)
	}
	sort.Strings(qualified)

	if s.renderer == nil {
		return
	}
	outbounds := s.subscribe()
	data, err := s.renderer.RenderWithQualifiedNodes(outbounds, qualified)
	if err != nil {
		slog.Error("nodescorer: re-render failed", "err", err)
		return
	}
	if err := writeAtomic(s.renderer.Path(), data); err != nil {
		slog.Error("nodescorer: write config failed", "err", err)
		return
	}
	if err := s.mihomoHotReload(ctx); err != nil {
		slog.Error("nodescorer: hot-reload failed", "err", err)
		return
	}
	slog.Info("nodescorer: pool updated + hot-reloaded",
		"members", len(qualified), "qualified", qualified)
}

func (s *Scorer) mihomoHotReload(ctx context.Context) error {
	body := []byte(`{"path":"` + s.renderer.Path() + `"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		"http://"+s.apiAddr+"/configs?force=true",
		jsonBodyReader(body))
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
		return fmt.Errorf("PUT /configs: HTTP %d", resp.StatusCode)
	}
	return nil
}

// filterCandidates returns the subset of outbound tags that:
//   - are node-bearing (not selector/urltest/direct/etc.)
//   - match the NodePattern regexp
//   - exist as known entries in mihomo's /proxies map
//
// Using AllOutbounds() as source (not us-pool.all) means evicted nodes
// are still in the candidate set and can recover + be readmitted.
func filterCandidates(outbounds []subscribe.Outbound, nodePattern string, proxies map[string]map[string]interface{}) []string {
	var re *regexp.Regexp
	if nodePattern != "" {
		var err error
		re, err = regexp.Compile(nodePattern)
		if err != nil {
			re = nil
		}
	}
	var out []string
	for _, o := range outbounds {
		tag := o.Tag()
		if tag == "" {
			continue
		}
		if re != nil && !re.MatchString(tag) {
			continue
		}
		// Verify mihomo knows about this proxy (it may not if the subscription
		// hasn't been loaded yet or the proxy was removed).
		if _, ok := proxies[tag]; !ok {
			continue
		}
		out = append(out, tag)
	}
	return out
}

func (s *Scorer) fetchProxies(ctx context.Context) (map[string]map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+s.apiAddr+"/proxies", nil)
	if err != nil {
		return nil, err
	}
	if s.apiSecret != "" {
		req.Header.Set("Authorization", "Bearer "+s.apiSecret)
	}
	resp, err := s.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Proxies map[string]map[string]interface{} `json:"proxies"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Proxies, nil
}

func (s *Scorer) fetchConnections(ctx context.Context) ([]map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+s.apiAddr+"/connections", nil)
	if err != nil {
		return nil, err
	}
	if s.apiSecret != "" {
		req.Header.Set("Authorization", "Bearer "+s.apiSecret)
	}
	resp, err := s.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Connections []map[string]interface{} `json:"connections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Connections, nil
}
