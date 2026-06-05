package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
	"github.com/leap-gateway/leap-gateway/internal/nodeinfo"
	"github.com/leap-gateway/leap-gateway/internal/singbox"
	"github.com/leap-gateway/leap-gateway/internal/watchdog"
)

// activeDTO is the comprehensive "what's happening on this node" view. The
// shape matches the API contract agreed during design — keep field names
// stable.
type activeDTO struct {
	Node        nodeinfo.NodeInfo    `json:"node"`
	FeiLian     nodeinfo.FeiLianInfo `json:"feilian"`
	Leap        leapDTO              `json:"leap"`
	ActiveProxy activeProxyDTO       `json:"active_proxy"`
	Watchdog    watchdog.Snapshot    `json:"watchdog"`
}

type leapDTO struct {
	GatewayVersion     string            `json:"gateway_version"`
	SingBoxVersion     string            `json:"singbox_version"`
	Services           map[string]string `json:"services"`
	SubscriptionsCount int               `json:"subscriptions_count"`
	NodesParsed        int               `json:"nodes_parsed"`
	LastRefresh        string            `json:"last_refresh,omitempty"`
}

type activeProxyDTO struct {
	Now           string          `json:"now"`            // currently-selected airport node tag (mirrors active pool's `now`)
	DelayMs       int             `json:"delay_ms"`       // mirrors active pool's selected node's last delay
	LastCheck     string          `json:"last_check,omitempty"`
	PoolSize      int             `json:"pool_size"` // size of the active urltest pool (primary or backup)
	PoolFilter    string          `json:"pool_filter,omitempty"`
	HistoryLen    int             `json:"history_len"`
	Reachable     bool            `json:"reachable"`      // whether clash-api responded
	ActiveURLTest string          `json:"active_urltest"` // urltest-primary or urltest-backup
	Pools         []poolStatusDTO `json:"pools,omitempty"`
}

type poolStatusDTO struct {
	Tag      string          `json:"tag"`       // urltest-primary | urltest-backup
	Now      string          `json:"now"`       // its currently-selected member
	PoolSize int             `json:"pool_size"`
	Active   bool            `json:"active"` // whether "out" selector points at this pool
	// Egress is the IP+geo seen by the public internet when traffic exits via
	// this pool's currently-selected member. Sampled lazily through a
	// pool-pinned HTTP inbound (see internal/singbox/renderer.go's
	// LeapInternalProxyURL / LeapInternalBackupProxyURL); cached 60s. nil =
	// no probe has succeeded yet (cold start, or upstream unreachable).
	Egress *egressInfo `json:"egress,omitempty"`
	// Nodes is per-member latency from clash-api's last urltest measurement.
	// One entry per `all` member of the urltest. Empty list = clash-api
	// unreachable or pool freshly rendered, no measurements yet.
	Nodes []nodeStatusDTO `json:"nodes,omitempty"`
}

type nodeStatusDTO struct {
	Tag       string `json:"tag"`
	DelayMs   int    `json:"delay_ms"`              // 0 if no measurement available
	LastCheck string `json:"last_check,omitempty"` // RFC3339 timestamp from clash-api
}

func (s *Server) handleProxiesActive(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	ni := s.deps.NodeInfo.Snapshot()

	// Subscriptions/nodes from the in-memory subscribe cache.
	results, last := s.deps.Subscribe.Snapshot()
	nodesParsed := 0
	for _, rs := range results {
		nodesParsed += len(rs.Outbounds)
	}
	leap := leapDTO{
		GatewayVersion:     ni.Leap.GatewayVersion,
		SingBoxVersion:     ni.Leap.SingBoxVersion,
		Services:           ni.Leap.Services,
		SubscriptionsCount: len(s.deps.Cfg.Subscriptions),
		NodesParsed:        nodesParsed,
	}
	if !last.IsZero() {
		leap.LastRefresh = last.UTC().Format(time.RFC3339)
	}

	// Live clash-api hit for the active proxy state.
	ap := s.queryActiveProxy(ctx)

	wdSnap := watchdog.Snapshot{}
	if s.deps.Watchdog != nil {
		wdSnap = s.deps.Watchdog.Snapshot()
	}

	writeJSON(w, http.StatusOK, activeDTO{
		Node:        ni.Node,
		FeiLian:     ni.FeiLian,
		Leap:        leap,
		ActiveProxy: ap,
		Watchdog:    wdSnap,
	})
}

// queryActiveProxy walks clash-api selector → urltest → leaf node:
//
//  1. GET /proxies — bulk dump of every proxy + its history (1 round-trip)
//  2. From it: pluck the "out" selector's `now` field (primary | backup | direct)
//  3. For each urltest-* member, list its `all` and per-node latency from
//     each member's history
//  4. Per pool: kick the lazy egress probe through that pool's pinned
//     loopback HTTP inbound (urltest-primary → 11080, urltest-backup → 11081)
//
// Returns Reachable=false on any clash-api error so the rest of the response
// is still useful.
func (s *Server) queryActiveProxy(ctx context.Context) activeProxyDTO {
	api := s.deps.Cfg.SingBox.ClashAPI
	pf := s.deps.Cfg.SingBox.URLTest.NodePattern

	client := &http.Client{Timeout: 2 * time.Second}

	var bulk struct {
		Proxies map[string]struct {
			Type    string         `json:"type"`
			Now     string         `json:"now"`
			All     []string       `json:"all"`
			History []historyEntry `json:"history"`
		} `json:"proxies"`
	}
	if err := getJSON(ctx, client, api, "/proxies", &bulk); err != nil {
		return activeProxyDTO{PoolFilter: pf}
	}

	out, ok := bulk.Proxies["out"]
	if !ok {
		return activeProxyDTO{PoolFilter: pf}
	}
	dto := activeProxyDTO{
		PoolFilter:    pf,
		Reachable:     true,
		ActiveURLTest: out.Now,
	}

	for _, name := range out.All {
		pool, ok := bulk.Proxies[name]
		if !ok {
			continue
		}
		// Accept both sing-box's URLTest pools (urltest-primary/-backup) and
		// mihomo's LoadBalance pool (us-pool). Skip leaf nodes / DIRECT /
		// REJECT — those have other types.
		if pool.Type != "URLTest" && pool.Type != "LoadBalance" {
			continue
		}
		ps := poolStatusDTO{
			Tag:      name,
			Now:      pool.Now,
			PoolSize: len(pool.All),
			Active:   name == out.Now,
		}
		// Per-member delay from each node's own history.
		ps.Nodes = make([]nodeStatusDTO, 0, len(pool.All))
		for _, member := range pool.All {
			n := nodeStatusDTO{Tag: member}
			if mp, ok := bulk.Proxies[member]; ok && len(mp.History) > 0 {
				h := mp.History[len(mp.History)-1]
				n.DelayMs = h.Delay
				n.LastCheck = h.Time
			}
			ps.Nodes = append(ps.Nodes, n)
		}
		// Egress probe — per-pool pinned proxy URL. Skip if no URL is wired
		// for this pool tag (defensive; only urltest-primary / urltest-backup
		// have inbounds today).
		if proxyURL := proxyURLForPool(name); proxyURL != "" {
			ps.Egress = s.egress.GetOrRefresh(name, proxyURL, pool.Now)
		}
		dto.Pools = append(dto.Pools, ps)
		if ps.Active {
			dto.Now = pool.Now
			dto.PoolSize = len(pool.All)
			dto.HistoryLen = len(pool.History)
			if n := len(pool.History); n > 0 {
				last := pool.History[n-1]
				dto.DelayMs = last.Delay
				dto.LastCheck = last.Time
			}
		}
	}

	// Edge case: selector points at "direct" or something not starting with
	// urltest — Now stays "", PoolSize 0, but ActiveURLTest is informative.
	return dto
}

// proxyURLForPool maps an urltest pool tag to the loopback HTTP-proxy URL
// the renderer pinned to it. Used by queryActiveProxy to feed the egress
// cache. Returns "" for pools that don't have a pinned inbound, in which
// case the egress probe is skipped (the cache simply won't populate).
//
// Note: urltest-primary uses LeapInternalPrimaryProxyURL (11082), NOT
// LeapInternalProxyURL (11080). The latter follows the `out` selector and
// would silently route the probe via whatever pool is currently selected
// — defeating the whole point of "what's primary's egress" when watchdog
// has flipped to backup.
func proxyURLForPool(poolTag string) string {
	switch poolTag {
	case "urltest-primary":
		return singbox.LeapInternalPrimaryProxyURL
	case "urltest-backup":
		return singbox.LeapInternalBackupProxyURL
	}
	return ""
}

// getJSON is a tiny helper that does the auth+timeout+decode dance once.
func getJSON(ctx context.Context, client *http.Client, api config.ClashAPIConfig, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+api.ExternalController+path, nil)
	if err != nil {
		return err
	}
	if api.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+api.Secret)
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("clash-api %s: status %d", path, res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

type historyEntry struct {
	Time  string `json:"time"`
	Delay int    `json:"delay"`
}

// proxiesSelectDTO is the body of POST /api/proxies/select.
type proxiesSelectDTO struct {
	Selector string `json:"selector"`
}

// validSelectors is the closed set of names POST /api/proxies/select accepts —
// matches what the renderer puts into the "out" selector's outbounds list
// (see internal/singbox/renderer.go:302). "direct" lets operators bypass the
// airport entirely as an emergency override.
var validSelectors = map[string]bool{
	"urltest-primary": true,
	"urltest-backup":  true,
	"direct":          true,
}

// handleProxiesSelect manually flips the "out" selector to the requested pool.
// Useful as an operator override when the watchdog's automatic primary→backup
// failover hasn't fired (or shouldn't) but you want to switch right now.
//
// The override is NOT sticky — the watchdog keeps running and may flip again:
//   - manual → urltest-backup, primary healthy: watchdog's recovery goroutine
//     (see watchdog.runPrimaryRecovery) eventually flips back after
//     primary_recovery_threshold consecutive healthy probes.
//   - manual → urltest-primary, primary failing: watchdog will flip to backup
//     again after fail_threshold consecutive failures.
//   - manual → direct: stays direct until either side flips it back.
func (s *Server) handleProxiesSelect(w http.ResponseWriter, r *http.Request) {
	var dto proxiesSelectDTO
	if err := json.NewDecoder(r.Body).Decode(&dto); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	dto.Selector = strings.TrimSpace(dto.Selector)
	if !validSelectors[dto.Selector] {
		http.Error(w, fmt.Sprintf("invalid selector %q (want urltest-primary | urltest-backup | direct)", dto.Selector), http.StatusBadRequest)
		return
	}

	// Pre-check that the selector is registered on this node before issuing
	// the PUT. clash-api returns 400 (not 404) for unregistered members,
	// which would surface as an unhelpful 500 here. Reading the "out"
	// selector's `all` list lets us return a clean 404 with operator-level
	// context (the most common cause is single-subscription deploys having
	// no urltest-backup).
	httpc := &http.Client{Timeout: 2 * time.Second}
	var sel struct {
		All []string `json:"all"`
	}
	if err := getJSON(r.Context(), httpc, s.deps.Cfg.SingBox.ClashAPI, "/proxies/out", &sel); err != nil {
		http.Error(w, "clash-api read /proxies/out: "+err.Error(), http.StatusInternalServerError)
		return
	}
	registered := false
	for _, m := range sel.All {
		if m == dto.Selector {
			registered = true
			break
		}
	}
	if !registered {
		http.Error(w, fmt.Sprintf("selector %q not registered on sing-box (registered: %v — single-subscription deploys have no urltest-backup)", dto.Selector, sel.All), http.StatusNotFound)
		return
	}

	if err := setOutSelector(r.Context(), s.deps.Cfg.SingBox.ClashAPI, dto.Selector); err != nil {
		http.Error(w, "clash-api: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Echo the fresh state so the operator sees the post-switch reality.
	s.handleProxiesActive(w, r)
}

// setOutSelector PUTs to clash-api /proxies/out to pin the "out" selector
// at the named member. Mirrors watchdog.setSelector but lives here so the
// API package doesn't need to depend on the watchdog package's internals.
func setOutSelector(ctx context.Context, api config.ClashAPIConfig, member string) error {
	body, _ := json.Marshal(map[string]string{"name": member})
	u := "http://" + api.ExternalController + "/proxies/" + url.PathEscape("out")
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if api.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+api.Secret)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("PUT /proxies/out: status %d", res.StatusCode)
	}
	return nil
}
