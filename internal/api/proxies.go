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
	"github.com/leap-gateway/leap-gateway/internal/dataplane"
	"github.com/leap-gateway/leap-gateway/internal/nodeinfo"
)

// activeDTO is the comprehensive "what's happening on this node" view. The
// shape matches the API contract agreed during design — keep field names
// stable.
type activeDTO struct {
	Node        nodeinfo.NodeInfo    `json:"node"`
	FeiLian     nodeinfo.FeiLianInfo `json:"feilian"`
	Leap        leapDTO              `json:"leap"`
	ActiveProxy activeProxyDTO       `json:"active_proxy"`
}

type leapDTO struct {
	GatewayVersion     string            `json:"gateway_version"`
	Engine             string            `json:"engine"`
	EngineVersion      string            `json:"engine_version"`
	Services           map[string]string `json:"services"`
	SubscriptionsCount int               `json:"subscriptions_count"`
	NodesParsed        int               `json:"nodes_parsed"`
	LastRefresh        string            `json:"last_refresh,omitempty"`
}

type activeProxyDTO struct {
	Now           string          `json:"now"`            // currently-selected airport node tag inside the active pool (mirrors pool.now)
	DelayMs       int             `json:"delay_ms"`       // mirrors active pool's selected node's last delay
	LastCheck     string          `json:"last_check,omitempty"`
	PoolSize      int             `json:"pool_size"` // size of the active load-balance pool
	PoolFilter    string          `json:"pool_filter,omitempty"`
	HistoryLen    int             `json:"history_len"`
	Reachable     bool            `json:"reachable"`      // whether clash-api responded
	ActiveURLTest string          `json:"active_urltest"` // mihomo: "us-pool" (kept name for backward compat with dashboard scrapers)
	Pools         []poolStatusDTO `json:"pools,omitempty"`
}

type poolStatusDTO struct {
	Tag      string          `json:"tag"`       // mihomo: "us-pool"
	Now      string          `json:"now"`       // its currently-selected member
	PoolSize int             `json:"pool_size"`
	Active   bool            `json:"active"` // whether "out" selector points at this pool
	// Egress is the IP+geo seen by the public internet when traffic exits
	// via this pool's currently-selected member. Probed through the leap-
	// internal HTTP proxy (LeapInternalProxyURL = 127.0.0.1:11080), which
	// follows mihomo's "out" selector → us-pool. Cached 60s. nil = no
	// probe has succeeded yet (cold start, or upstream unreachable).
	Egress *egressInfo `json:"egress,omitempty"`
	// Nodes is per-member latency from clash-api's last url-test measurement.
	// One entry per `all` member of the pool. Empty list = clash-api
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
		Engine:             ni.Leap.Engine,
		EngineVersion:      ni.Leap.EngineVersion,
		Services:           ni.Leap.Services,
		SubscriptionsCount: len(s.deps.Cfg.Subscriptions),
		NodesParsed:        nodesParsed,
	}
	if !last.IsZero() {
		leap.LastRefresh = last.UTC().Format(time.RFC3339)
	}

	// Live clash-api hit for the active proxy state.
	ap := s.queryActiveProxy(ctx)

	writeJSON(w, http.StatusOK, activeDTO{
		Node:        ni.Node,
		FeiLian:     ni.FeiLian,
		Leap:        leap,
		ActiveProxy: ap,
	})
}

// queryActiveProxy walks clash-api selector → load-balance pool → leaf node:
//
//  1. GET /proxies — bulk dump of every proxy + its history (1 round-trip)
//  2. From it: pluck the "out" selector's `now` field (us-pool / pin / DIRECT)
//  3. For us-pool, list its members and per-node latency
//  4. Kick the lazy egress probe through LeapInternalProxyURL (which goes
//     "out → us-pool" via mihomo, so the IP we sample IS us-pool's egress)
//
// Returns Reachable=false on any clash-api error so the rest of the response
// is still useful.
func (s *Server) queryActiveProxy(ctx context.Context) activeProxyDTO {
	api := s.deps.Cfg.DataPlane.ClashAPI
	pf := s.deps.Cfg.DataPlane.URLTest.NodePattern

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
		// mihomo's us-pool is type LoadBalance. Skip leaf nodes / DIRECT
		// / REJECT / Selector children — those have other types.
		if pool.Type != "LoadBalance" {
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
		// Egress probe — through the leap-internal HTTP proxy. mihomo
		// routes 11080 through "out → us-pool", so the IP we sample is
		// the pool's egress IP. Cached 60s by egressCache.
		if ps.Active {
			ps.Egress = s.egress.GetOrRefresh(name, dataplane.LeapInternalProxyURL, pool.Now)
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

	// Edge case: "out" points at "DIRECT" or "pin" (Selector type, filtered
	// out above) — Now stays "", PoolSize 0, ActiveURLTest reflects what was
	// selected.
	return dto
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
//
// Two-field shape so callers can flip ANY Selector group to any of its
// registered members — engine-agnostic by design:
//
//	{ "selector": "out", "name": "us-pool"   }   // mihomo: route via load-balance
//	{ "selector": "out", "name": "pin"       }   // mihomo: route via the manually-pinned node
//	{ "selector": "pin", "name": "yuyun/SG-01" } // mihomo: pick which node `pin` points at
//	{ "selector": "out", "name": "urltest-primary" } // sing-box: flip back to primary pool
//	{ "selector": "out", "name": "direct"    }   // either engine: emergency bypass
//
// Validation is dynamic — we GET /proxies/{selector} from clash-api and
// require {name} to be in its `all` list. No hardcoded selector or member
// names: works under both mihomo and sing-box renderers without code
// changes when groups evolve.
type proxiesSelectDTO struct {
	Selector string `json:"selector"`
	Name     string `json:"name"`
}

// handleProxiesSelect manually flips a Selector group at one of its
// registered members. Useful as an operator override (force route via a
// specific pool / node) or for emergency direct bypass.
//
// The override is NOT sticky:
//   - sing-box engine: the watchdog keeps running and may flip "out" back
//     to primary/backup on its own depending on health probes.
//   - mihomo engine: the NodeScorer manages us-pool MEMBERSHIP (which
//     nodes can carry traffic) but does not touch selector pointers, so
//     manual `out` / `pin` flips persist until the operator reverts them
//     or restarts the data plane.
func (s *Server) handleProxiesSelect(w http.ResponseWriter, r *http.Request) {
	var dto proxiesSelectDTO
	if err := json.NewDecoder(r.Body).Decode(&dto); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	dto.Selector = strings.TrimSpace(dto.Selector)
	dto.Name = strings.TrimSpace(dto.Name)
	if dto.Selector == "" || dto.Name == "" {
		http.Error(w, `body must include "selector" and "name" (e.g. {"selector":"out","name":"us-pool"})`, http.StatusBadRequest)
		return
	}

	// Validate against clash-api's live state. Avoids hardcoding selector
	// or member names per engine — the renderer decides what exists; we
	// just echo what's there. A 404 from clash-api means the selector
	// group isn't registered (engine difference, typo); a missing member
	// in `all` means the requested name isn't a child of that group.
	httpc := &http.Client{Timeout: 2 * time.Second}
	var sel struct {
		Type string   `json:"type"`
		All  []string `json:"all"`
	}
	if err := getJSON(r.Context(), httpc, s.deps.Cfg.DataPlane.ClashAPI, "/proxies/"+url.PathEscape(dto.Selector), &sel); err != nil {
		http.Error(w, fmt.Sprintf("selector %q not found on data plane: %v", dto.Selector, err), http.StatusNotFound)
		return
	}
	// Only Selector-type groups can be flipped manually. URLTest /
	// LoadBalance manage their own active member; PUT-ing a name there is
	// a no-op stub at best.
	if sel.Type != "Selector" {
		http.Error(w, fmt.Sprintf("selector %q has type %q (only Selector groups can be flipped)", dto.Selector, sel.Type), http.StatusBadRequest)
		return
	}
	registered := false
	for _, m := range sel.All {
		if m == dto.Name {
			registered = true
			break
		}
	}
	if !registered {
		http.Error(w, fmt.Sprintf("name %q not registered under selector %q (registered: %v)", dto.Name, dto.Selector, sel.All), http.StatusNotFound)
		return
	}

	if err := setSelector(r.Context(), s.deps.Cfg.DataPlane.ClashAPI, dto.Selector, dto.Name); err != nil {
		http.Error(w, "clash-api: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Echo the fresh state so the operator sees the post-switch reality.
	s.handleProxiesActive(w, r)
}

// setSelector PUTs to clash-api /proxies/{selector} to pin a Selector group
// at the named member. Engine-agnostic: works for mihomo's "out"/"pin" and
// sing-box's "out".
func setSelector(ctx context.Context, api config.ClashAPIConfig, selector, member string) error {
	body, _ := json.Marshal(map[string]string{"name": member})
	u := "http://" + api.ExternalController + "/proxies/" + url.PathEscape(selector)
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
		return fmt.Errorf("PUT /proxies/%s: status %d", selector, res.StatusCode)
	}
	return nil
}
