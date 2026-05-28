package watchdog

import (
	"testing"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

func TestAdvanceBackoff(t *testing.T) {
	w := &Watchdog{cfg: config.WatchdogConfig{
		Interval:   5 * time.Second,
		BackoffMax: 60 * time.Second,
	}}
	cases := []struct {
		curr time.Duration
		want time.Duration
	}{
		{5 * time.Second, 10 * time.Second},
		{10 * time.Second, 20 * time.Second},
		{20 * time.Second, 40 * time.Second},
		{40 * time.Second, 60 * time.Second}, // capped
		{60 * time.Second, 60 * time.Second}, // stays at cap
	}
	for _, c := range cases {
		got := w.advanceBackoff(c.curr)
		if got != c.want {
			t.Errorf("advanceBackoff(%v) = %v, want %v", c.curr, got, c.want)
		}
	}
}

func TestAdvanceBackoffDisabled(t *testing.T) {
	w := &Watchdog{cfg: config.WatchdogConfig{
		Interval:   5 * time.Second,
		BackoffMax: 0,
	}}
	if got := w.advanceBackoff(10 * time.Second); got != 5*time.Second {
		t.Errorf("BackoffMax=0 should always return Interval, got %v", got)
	}
}

func TestJitterRange(t *testing.T) {
	w := New(config.ClashAPIConfig{}, config.WatchdogConfig{
		Interval:      5 * time.Second,
		JitterPercent: 20,
	}, "")
	// 1000 samples — none should fall outside ±20% of 5s.
	min, max := time.Duration(1<<62), time.Duration(0)
	for i := 0; i < 1000; i++ {
		d := w.jitter(5 * time.Second)
		if d < min {
			min = d
		}
		if d > max {
			max = d
		}
	}
	if min < 4*time.Second {
		t.Errorf("jitter min %v < 4s — outside ±20%% range", min)
	}
	if max > 6*time.Second {
		t.Errorf("jitter max %v > 6s — outside ±20%% range", max)
	}
	if min == max {
		t.Errorf("jitter produced constant %v — RNG broken", min)
	}
}

func TestChainsContainOut(t *testing.T) {
	cases := []struct {
		chains []string
		want   bool
	}{
		{[]string{"yuyun/SG01", "urltest-primary", "out"}, true},
		{[]string{"yuyun/SG01", "urltest-backup", "out"}, true},
		{[]string{"direct"}, false},
		{[]string{}, false},
		{nil, false},
		{[]string{"out"}, true},
	}
	for _, c := range cases {
		if got := chainsContainOut(c.chains); got != c.want {
			t.Errorf("chainsContainOut(%v) = %v, want %v", c.chains, got, c.want)
		}
	}
}

func TestBoolDeref(t *testing.T) {
	if !boolDeref(nil, true) {
		t.Errorf("nil with default true should be true")
	}
	if boolDeref(nil, false) {
		t.Errorf("nil with default false should be false")
	}
	tr := true
	fa := false
	if !boolDeref(&tr, false) {
		t.Errorf("*true should be true regardless of default")
	}
	if boolDeref(&fa, true) {
		t.Errorf("*false should be false regardless of default")
	}
}
