package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// egressInfo is the public-facing IP + geo of one urltest pool's exit point,
// sampled by dialing an IP-echo service through that pool's pinned HTTP
// inbound. ViaNode is the pool's `now` at probe time; if the operator
// flipped selectors after the probe ran, GetOrRefresh marks Stale=true so
// the API consumer can decide whether the cached value is still meaningful.
type egressInfo struct {
	IP        string    `json:"ip"`
	Country   string    `json:"country,omitempty"`
	Region    string    `json:"region,omitempty"`
	City      string    `json:"city,omitempty"`
	Org       string    `json:"org,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	ViaNode   string    `json:"via_node,omitempty"`
	Stale     bool      `json:"stale,omitempty"`
}

const (
	egressTTL          = 60 * time.Second
	egressProbeTimeout = 12 * time.Second
	ipinfoURL          = "https://ipinfo.io/json"
	cfTraceURL         = "https://1.1.1.1/cdn-cgi/trace"
)

type egressEntry struct {
	info     *egressInfo
	inflight bool
}

// egressCache is keyed by pool tag (urltest-primary / urltest-backup) and
// holds the last successful probe result plus an inflight flag so two
// concurrent /api/proxies/active GETs don't both kick off probes.
type egressCache struct {
	mu      sync.Mutex
	entries map[string]*egressEntry
	// probeFn is overridable for tests; production wires it to probeEgress.
	probeFn func(ctx context.Context, proxyURL, viaNode string) (*egressInfo, error)
}

func newEgressCache() *egressCache {
	return &egressCache{
		entries: make(map[string]*egressEntry),
		probeFn: probeEgress,
	}
}

// GetOrRefresh returns the currently-cached egressInfo for poolTag. When
// cache is empty or older than egressTTL, kicks off a background probe (at
// most one in flight per pool); the in-flight probe updates the cache when
// it returns. Callers always get the freshest snapshot available right now,
// possibly nil on the very first call before any probe finishes.
//
// If currentNode != cached.ViaNode, the returned snapshot has Stale=true —
// the cached IP was sampled when a different pool member was selected, so
// it doesn't strictly represent the pool's current exit anymore (sing-box
// urltest may have flipped to a different node since).
func (c *egressCache) GetOrRefresh(poolTag, proxyURL, currentNode string) *egressInfo {
	c.mu.Lock()
	e := c.entries[poolTag]
	if e == nil {
		e = &egressEntry{}
		c.entries[poolTag] = e
	}
	stale := e.info == nil || time.Since(e.info.CheckedAt) > egressTTL
	startProbe := stale && !e.inflight
	if startProbe {
		e.inflight = true
	}
	var snap *egressInfo
	if e.info != nil {
		cp := *e.info
		snap = &cp
	}
	c.mu.Unlock()

	if startProbe {
		go c.runProbe(poolTag, proxyURL, currentNode)
	}

	if snap != nil && currentNode != "" && snap.ViaNode != currentNode {
		snap.Stale = true
	}
	return snap
}

// runProbe is the goroutine body that does the actual HTTP dial through
// the pool-pinned proxy and updates the cache entry under mu. Errors leave
// the previous cache value in place but flag it Stale=true.
func (c *egressCache) runProbe(poolTag, proxyURL, viaNode string) {
	ctx, cancel := context.WithTimeout(context.Background(), egressProbeTimeout)
	defer cancel()
	info, err := c.probeFn(ctx, proxyURL, viaNode)

	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[poolTag]
	e.inflight = false
	if err != nil {
		slog.Warn("egress: probe failed", "pool", poolTag, "via", viaNode, "err", err)
		if e.info != nil {
			e.info.Stale = true
		}
		return
	}
	e.info = info
	slog.Debug("egress: probed", "pool", poolTag, "via", viaNode, "ip", info.IP, "country", info.Country)
}

// probeEgress dials ipinfo.io first then falls back to cloudflare trace,
// both via the supplied HTTP-proxy URL. The two services have very
// different reliability profiles (ipinfo geo-rich + occasional 5xx/rate
// limit; cf trace minimal + extremely stable) so the fallback ladder
// gives both — useful operationally because the cf trace's `loc` field
// at least tells you the country code.
func probeEgress(ctx context.Context, proxyURL, viaNode string) (*egressInfo, error) {
	proxyU, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy %q: %w", proxyURL, err)
	}
	client := &http.Client{
		Timeout: egressProbeTimeout,
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(proxyU),
			DisableKeepAlives: true,
		},
	}

	var lastErr error
	for _, p := range []func(context.Context, *http.Client) (*egressInfo, error){probeIpinfo, probeCFTrace} {
		info, err := p(ctx, client)
		if err != nil {
			lastErr = err
			continue
		}
		info.CheckedAt = time.Now()
		info.ViaNode = viaNode
		return info, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no probe service produced a result")
	}
	return nil, lastErr
}

func probeIpinfo(ctx context.Context, client *http.Client) (*egressInfo, error) {
	body, err := getThrough(ctx, client, ipinfoURL)
	if err != nil {
		return nil, err
	}
	var raw struct {
		IP      string `json:"ip"`
		City    string `json:"city"`
		Region  string `json:"region"`
		Country string `json:"country"`
		Org     string `json:"org"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("ipinfo decode: %w", err)
	}
	if raw.IP == "" {
		return nil, fmt.Errorf("ipinfo: empty ip")
	}
	return &egressInfo{
		IP:      raw.IP,
		City:    raw.City,
		Region:  raw.Region,
		Country: raw.Country,
		Org:     raw.Org,
	}, nil
}

func probeCFTrace(ctx context.Context, client *http.Client) (*egressInfo, error) {
	body, err := getThrough(ctx, client, cfTraceURL)
	if err != nil {
		return nil, err
	}
	out := &egressInfo{}
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "ip":
			out.IP = v
		case "loc":
			out.Country = v
		}
	}
	if out.IP == "" {
		return nil, fmt.Errorf("cf-trace: empty ip")
	}
	return out, nil
}

func getThrough(ctx context.Context, client *http.Client, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "leap-gateway-egressprobe/0.1")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s: status %d", target, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<10))
}
