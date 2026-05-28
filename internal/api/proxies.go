package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
	"github.com/leap-gateway/leap-gateway/internal/nodeinfo"
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
	Now           string         `json:"now"`             // currently-selected airport node tag
	DelayMs       int            `json:"delay_ms"`
	LastCheck     string         `json:"last_check,omitempty"`
	PoolSize      int            `json:"pool_size"`       // size of the active urltest pool (primary or backup)
	PoolFilter    string         `json:"pool_filter,omitempty"`
	HistoryLen    int            `json:"history_len"`
	Reachable     bool           `json:"reachable"`        // whether clash-api responded
	ActiveURLTest string         `json:"active_urltest"`   // urltest-primary or urltest-backup
	Pools         []poolStatusDTO `json:"pools,omitempty"` // visibility into each urltest pool
}

type poolStatusDTO struct {
	Tag      string `json:"tag"`       // urltest-primary | urltest-backup
	Now      string `json:"now"`       // its currently-selected member
	PoolSize int    `json:"pool_size"`
	Active   bool   `json:"active"`    // whether "out" selector points at this pool
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
//  1. GET /proxies/out — which urltest member is currently in use
//     (urltest-primary, urltest-backup, or "direct")
//  2. GET /proxies/<that-urltest> — which airport node it's picked
//
// Also lists every urltest-* it can find so the operator can see backup pool
// state. Returns Reachable=false on any error so the rest of the response is
// still useful.
func (s *Server) queryActiveProxy(ctx context.Context) activeProxyDTO {
	api := s.deps.Cfg.SingBox.ClashAPI
	pf := s.deps.Cfg.SingBox.URLTest.NodePattern

	client := &http.Client{Timeout: 2 * time.Second}

	// Step 1: read selector "out".
	var selector struct {
		Now string   `json:"now"`
		All []string `json:"all"`
	}
	if err := getJSON(ctx, client, api, "/proxies/out", &selector); err != nil {
		return activeProxyDTO{PoolFilter: pf}
	}
	dto := activeProxyDTO{
		PoolFilter:    pf,
		Reachable:     true,
		ActiveURLTest: selector.Now,
	}

	// Step 2: enumerate every urltest-* member of the selector and probe each.
	for _, name := range selector.All {
		if !strings.HasPrefix(name, "urltest") {
			continue
		}
		var ut struct {
			Now     string         `json:"now"`
			All     []string       `json:"all"`
			History []historyEntry `json:"history"`
		}
		if err := getJSON(ctx, client, api, "/proxies/"+name, &ut); err != nil {
			continue
		}
		ps := poolStatusDTO{
			Tag:      name,
			Now:      ut.Now,
			PoolSize: len(ut.All),
			Active:   name == selector.Now,
		}
		dto.Pools = append(dto.Pools, ps)
		if ps.Active {
			dto.Now = ut.Now
			dto.PoolSize = len(ut.All)
			dto.HistoryLen = len(ut.History)
			if n := len(ut.History); n > 0 {
				last := ut.History[n-1]
				dto.DelayMs = last.Delay
				dto.LastCheck = last.Time
			}
		}
	}

	// Edge case: selector points at "direct" or something not starting with
	// urltest — Now stays "", PoolSize 0, but ActiveURLTest is informative.
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
