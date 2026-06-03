package dnspreload

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestNew_NilOnEmpty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		addr     string
		domains  []string
		interval time.Duration
	}{
		{"empty domains", "1.2.3.4:53", nil, 1 * time.Minute},
		{"empty domains slice", "1.2.3.4:53", []string{}, 1 * time.Minute},
		{"zero interval", "1.2.3.4:53", []string{"x"}, 0},
		{"negative interval", "1.2.3.4:53", []string{"x"}, -1},
		{"empty addr", "", []string{"x"}, time.Minute},
	}
	for _, c := range cases {
		got := New(c.addr, c.domains, c.interval)
		if got != nil {
			t.Errorf("New(%s) = non-nil, want nil", c.name)
		}
	}
}

// TestPreloader_DispatchesQueriesToConfiguredAddr opens a real UDP socket,
// counts arriving packets per source address, and verifies the Preloader
// fires one query per domain per sweep at the configured target.
//
// The UDP listener doesn't parse DNS — it just counts packets. The
// resolver's Dial succeeds (UDP connect always does) but the lookup
// times out reading the response (since we don't reply); the resolver
// then returns an error, which is fine — we only care that the request
// went out. Each domain logs an "ok=0 fail=1" line per sweep but the
// preloader keeps going.
func TestPreloader_DispatchesQueriesToConfiguredAddr(t *testing.T) {
	t.Parallel()
	addr, hits, stop := startUDPCounter(t)
	defer stop()

	domains := []string{"a.test.invalid", "b.test.invalid", "c.test.invalid"}
	p := New(addr, domains, 60*time.Millisecond)
	if p == nil {
		t.Fatal("New returned nil")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run.run waits 3s before its first sweep — too long for a unit test.
	// Bypass it by calling sweep() directly in a tight loop, two iterations.
	p.sweep(ctx)
	p.sweep(ctx)

	// At least len(domains) packets per sweep × 2 sweeps = 6 packets.
	// Allow some slack since net.Resolver may retry.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hits.Load() >= int64(2*len(domains)) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("expected ≥%d UDP packets received at preload target, got %d",
		2*len(domains), hits.Load())
}

// startUDPCounter binds a UDP socket on a random localhost port, increments
// hits on every packet received, and returns the addr + counter + stop fn.
func startUDPCounter(t *testing.T) (addr string, hits *atomic.Int64, stop func()) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	hits = &atomic.Int64{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 512)
		for {
			_ = pc.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			n, _, err := pc.ReadFrom(buf)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				return // socket closed
			}
			if n > 0 {
				hits.Add(1)
			}
		}
	}()
	stop = func() {
		_ = pc.Close()
		<-done
	}
	return pc.LocalAddr().String(), hits, stop
}
