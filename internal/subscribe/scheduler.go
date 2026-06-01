package subscribe

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Scheduler periodically invokes runFn at the configured interval. The
// interval is mutable at runtime via SetInterval; setting it to 0 (or any
// non-positive duration) pauses the loop, leaving only manual triggers.
//
// Run blocks until ctx is cancelled and is meant to be launched in its own
// goroutine. SetInterval / Interval are safe to call from any goroutine.
type Scheduler struct {
	mu       sync.Mutex
	interval time.Duration
	setCh    chan time.Duration
	runFn    func(context.Context) error
}

// NewScheduler builds a paused-or-armed scheduler that will call runFn every
// `initial` once Run is started. initial<=0 starts paused.
func NewScheduler(initial time.Duration, runFn func(context.Context) error) *Scheduler {
	return &Scheduler{
		interval: initial,
		setCh:    make(chan time.Duration, 1),
		runFn:    runFn,
	}
}

// Interval returns the currently-configured interval. 0 means paused.
func (s *Scheduler) Interval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interval
}

// SetInterval changes the interval and signals Run to re-arm. d<=0 pauses.
// Concurrent SetInterval calls are serialized via s.mu so the channel is
// never full when we push — Run reads s.Interval() after the wakeup, so even
// if two pushes coalesce in the channel buffer, the value Run reads is the
// latest one anyway.
func (s *Scheduler) SetInterval(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interval = d
	// drain any pending older value, then push — both under s.mu so a
	// concurrent SetInterval can't observe a full channel.
	select {
	case <-s.setCh:
	default:
	}
	s.setCh <- d
}

// Run drives the loop. Returns when ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	var timer *time.Timer
	arm := func(d time.Duration) {
		if timer != nil {
			timer.Stop()
			timer = nil
		}
		if d <= 0 {
			return
		}
		timer = time.NewTimer(d)
	}
	arm(s.Interval())
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		var fire <-chan time.Time
		if timer != nil {
			fire = timer.C
		}
		select {
		case <-ctx.Done():
			return
		case d := <-s.setCh:
			arm(d)
		case <-fire:
			if err := s.runFn(ctx); err != nil {
				slog.Warn("scheduled refresh failed", "err", err)
			}
			arm(s.Interval())
		}
	}
}
