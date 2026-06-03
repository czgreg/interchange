// Package dnspreload keeps overseas-bound DNS records warm in the local
// sing-box DNS cache so the first FeiLian client to access a popular
// domain doesn't pay the ~400ms cross-border DoH cold-resolve cost.
//
// Mechanism: on a fixed cadence, send a no-op A query for every preload
// domain straight to sing-box's DNS listener (tun0_gateway_ip:53). The
// query path inside sing-box matches what real client traffic would
// trigger — fakeip allocation + remote DoH lookup with detour=out — so
// the cache entry that lands is exactly the one the first real client
// would otherwise have had to wait for.
package dnspreload

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Preloader periodically dispatches DNS A queries against the local
// sing-box DNS server to keep its cache populated for a curated list of
// overseas domains.
type Preloader struct {
	dnsAddr  string
	domains  []string
	interval time.Duration
}

// New builds a Preloader that targets singboxDNSAddr (host:port — typically
// "<tun0_gateway_ip>:53") and refreshes each domain every interval. Empty
// domains or interval<=0 returns nil so the caller can `if p != nil` guard.
func New(singboxDNSAddr string, domains []string, interval time.Duration) *Preloader {
	if len(domains) == 0 || interval <= 0 {
		return nil
	}
	if singboxDNSAddr == "" {
		return nil
	}
	cp := make([]string, len(domains))
	copy(cp, domains)
	return &Preloader{
		dnsAddr:  singboxDNSAddr,
		domains:  cp,
		interval: interval,
	}
}

// Run blocks until ctx is cancelled. Designed to be launched in its own
// goroutine. First sweep happens 3s after start so sing-box's DNS server
// has time to come up; subsequent sweeps fire every Preloader.interval.
func (p *Preloader) Run(ctx context.Context) {
	if p == nil {
		return
	}
	slog.Info("dnspreload: starting",
		"target", p.dnsAddr, "domains", len(p.domains), "interval", p.interval)

	// Wait briefly so sing-box DNS is listening before the first sweep.
	select {
	case <-ctx.Done():
		return
	case <-time.After(3 * time.Second):
	}
	p.sweep(ctx)

	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.sweep(ctx)
		}
	}
}

// sweep fires every preload domain once through a goroutine pool. Each
// individual lookup gets a 5s budget — far longer than even a cold
// cross-border DoH (~400ms) but short enough that one bad domain can't
// hang the whole sweep cadence.
func (p *Preloader) sweep(ctx context.Context) {
	resolver := p.resolver()
	const concurrency = 8
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var ok, fail int64
	var mu sync.Mutex
	for _, d := range p.domains {
		wg.Add(1)
		sem <- struct{}{}
		go func(domain string) {
			defer wg.Done()
			defer func() { <-sem }()

			lookCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			_, err := resolver.LookupHost(lookCtx, domain)
			mu.Lock()
			if err != nil {
				fail++
			} else {
				ok++
			}
			mu.Unlock()
			if err != nil {
				slog.Debug("dnspreload: lookup failed", "domain", domain, "err", err)
			}
		}(d)
	}
	wg.Wait()
	slog.Info("dnspreload: sweep done", "ok", ok, "fail", fail, "total", len(p.domains))
}

// resolver builds a net.Resolver pinned at sing-box's DNS server. We
// override Dial because the system /etc/resolv.conf may point elsewhere —
// the whole point is to query OUR sing-box, not whatever DNS the host
// uses for itself.
func (p *Preloader) resolver() *net.Resolver {
	addr := p.dnsAddr
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			c, err := d.DialContext(ctx, "udp", addr)
			if err != nil {
				return nil, fmt.Errorf("dial %s: %w", addr, err)
			}
			return c, nil
		},
	}
}
