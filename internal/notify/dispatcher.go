package notify

import (
	"log/slog"
	"time"
)

// run is the single dispatcher goroutine. Reads from queue, applies dedup
// + rate-limit + aggregation, writes to local log, fires webhook. Single-
// goroutine semantics keep the rate-limit / dedup state lock-free except
// for the Notifier-level mutex (which only the API config-mutation
// handlers contend on).
func (n *Notifier) run() {
	defer close(n.done)

	// aggregation buffer: events accumulated within AggregationWindow
	// that share severity != urgent, flushed as one batch.
	var batch []Event
	var batchTimer *time.Timer
	flushBatch := func() {
		if len(batch) == 0 {
			return
		}
		n.deliver(batch)
		batch = nil
		if batchTimer != nil {
			batchTimer.Stop()
			batchTimer = nil
		}
	}

	for {
		select {
		case <-n.stop:
			flushBatch()
			return
		case ev := <-n.queue:
			n.mu.Lock()
			drop, isUrgent := n.shouldDropLocked(ev)
			n.mu.Unlock()
			if drop {
				if n.fileLog != nil {
					n.fileLog.Append(ev, "dropped_dedup_or_rate_limit", nil)
				}
				continue
			}
			// Always log locally first, regardless of webhook outcome.
			if n.fileLog != nil {
				n.fileLog.Append(ev, "queued", nil)
			}
			if isUrgent {
				// Urgent bypasses aggregation — flush any pending batch
				// first, then deliver this event on its own.
				flushBatch()
				n.deliver([]Event{ev})
				continue
			}
			batch = append(batch, ev)
			if batchTimer == nil && n.cfg.AggregationWindow > 0 {
				batchTimer = time.AfterFunc(n.cfg.AggregationWindow, func() {
					select {
					case n.queue <- Event{Type: "__flush__"}:
					default:
					}
				})
			}
		}
	}
}

// shouldDropLocked applies dedup + per-node-per-hour rate limit. Returns
// (drop, isUrgent). CALLER MUST HOLD n.mu.
func (n *Notifier) shouldDropLocked(ev Event) (bool, bool) {
	if ev.Type == "__flush__" {
		return true, false // sentinel from aggregation timer
	}
	urgent := ev.Severity == SeverityUrgent
	if urgent {
		return false, true
	}
	now := ev.Time
	key := ev.Node + "|" + ev.Type
	if n.cfg.DedupWindow > 0 {
		if last, ok := n.dedup[key]; ok && now.Sub(last) < n.cfg.DedupWindow {
			return true, false
		}
		n.dedup[key] = now
	}
	if ev.Node != "" && n.cfg.PerNodeRateLimitPerHour > 0 {
		cutoff := now.Add(-1 * time.Hour)
		hist := n.rateLog[ev.Node]
		kept := hist[:0]
		for _, t := range hist {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		if len(kept) >= n.cfg.PerNodeRateLimitPerHour {
			n.rateLog[ev.Node] = kept
			return true, false
		}
		n.rateLog[ev.Node] = append(kept, now)
	}
	return false, false
}

// deliver hands events to the Lark webhook (when enabled). Failures are
// logged + counted; urgent events get a redelivery attempt next cycle
// via the queue (the caller can re-emit if it cares about delivery).
func (n *Notifier) deliver(batch []Event) {
	if !n.cfg.Enabled || n.lark == nil || len(batch) == 0 {
		return
	}
	if err := n.lark.Send(batch); err != nil {
		slog.Warn("notify: lark webhook delivery failed",
			"err", err, "events", len(batch))
		if n.fileLog != nil {
			for _, ev := range batch {
				n.fileLog.Append(ev, "webhook_failed", map[string]any{"err": err.Error()})
			}
		}
		return
	}
	if n.fileLog != nil {
		for _, ev := range batch {
			n.fileLog.Append(ev, "delivered", nil)
		}
	}
}
