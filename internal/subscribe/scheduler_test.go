package subscribe

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestScheduler_PeriodicAndPauseResume(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	sched := NewScheduler(40*time.Millisecond, func(_ context.Context) error {
		calls.Add(1)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sched.Run(ctx)
		close(done)
	}()

	// Periodic: ~250ms / 40ms ≈ 6 ticks; allow slack for scheduler jitter.
	time.Sleep(250 * time.Millisecond)
	if got := calls.Load(); got < 4 {
		t.Fatalf("expected >=4 ticks in 250ms at 40ms interval, got %d", got)
	}

	// Pause: SetInterval(0) ⇒ no further ticks.
	sched.SetInterval(0)
	frozen := calls.Load()
	time.Sleep(150 * time.Millisecond)
	if got := calls.Load(); got != frozen {
		t.Fatalf("expected ticks frozen at %d after SetInterval(0), got %d", frozen, got)
	}
	if sched.Interval() != 0 {
		t.Fatalf("Interval()=%v after SetInterval(0)", sched.Interval())
	}

	// Resume: SetInterval(40ms) ⇒ ticks pick up again.
	sched.SetInterval(40 * time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	if got := calls.Load(); got <= frozen {
		t.Fatalf("expected ticks to resume after SetInterval(40ms), still at %d", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestScheduler_StartsPausedWhenZero(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	sched := NewScheduler(0, func(_ context.Context) error {
		calls.Add(1)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sched.Run(ctx)

	time.Sleep(150 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("expected 0 ticks when starting with interval=0, got %d", got)
	}
}
