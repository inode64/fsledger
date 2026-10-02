package daemon

import (
	"context"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/reportclock"
)

// ScanStatus reports observation work, independently of alert delivery.
type ScanStatus struct {
	Next               time.Time     `json:"next"`
	NextFull           time.Time     `json:"next_full"`
	Started            time.Time     `json:"started"`
	Finished           time.Time     `json:"finished"`
	Result             string        `json:"result"`
	Duration           time.Duration `json:"duration_ns"`
	Observed           int64         `json:"observed"`
	Hashed             int64         `json:"hashed"`
	Skipped            uint64        `json:"skipped"`
	SkippedApproximate bool          `json:"skipped_approximate"`
	Initialized        bool          `json:"initialized"`
	Full               bool          `json:"full"`
}

type scanClock struct {
	next        time.Time
	calendar    *reportclock.Schedule
	interval    time.Duration
	skipped     uint64
	approximate bool
}

type scanSchedule struct {
	ordinary scanClock
	full     scanClock
}

func newScanClock(now time.Time, interval time.Duration, expression, zone string) (scanClock, error) {
	clock := scanClock{interval: interval, next: now.Add(interval), calendar: nil, skipped: 0, approximate: false}

	if expression != "" {
		var err error

		clock.calendar, err = reportclock.Parse(expression, zone)
		if err != nil {
			return scanClock{}, err
		}

		clock.next = clock.calendar.Next(now)
	}

	return clock, nil
}

func (clock *scanClock) due(now time.Time) bool {
	if now.Before(clock.next) || clock.next.IsZero() {
		return false
	}

	clock.advance(now, true)

	return true
}

func (clock *scanClock) advance(now time.Time, consumed bool) {
	if clock.calendar == nil {
		clock.next = now.Add(clock.interval)

		return
	}

	// A suspended machine must not spend unbounded time enumerating missed minutes.
	const countLimit = 1024

	for count := 0; !clock.next.IsZero() && !clock.next.After(now); count++ {
		if !consumed {
			clock.skipped++
		}

		consumed = false

		if count == countLimit {
			clock.approximate = true

			break
		}

		clock.next = clock.calendar.Next(clock.next)
	}

	clock.next = clock.calendar.Next(now)
}

func (w *worker) prepareScanSchedule(now time.Time) error {
	ordinary, err := newScanClock(now, w.cfg.Watch.Reconcile.Interval,
		w.cfg.Watch.Reconcile.Schedule, w.cfg.Watch.Reconcile.Timezone)
	if err != nil {
		return err
	}

	if ordinary.calendar == nil {
		ordinary.next = now.Add(reconcileOffset(w.name, ordinary.interval))
	}

	full, err := newScanClock(now, w.cfg.Integrity.Hash.FullScanInterval,
		w.cfg.Integrity.Hash.FullScanSchedule, w.cfg.Integrity.Hash.FullScanTimezone)
	if err != nil {
		return err
	}

	w.scans = &scanSchedule{ordinary: ordinary, full: full}
	w.updateScanSchedule()

	return nil
}

func (w *worker) checkScanSchedule(now time.Time) {
	if w.scans == nil {
		return
	}

	ordinary, full := w.scans.ordinary.due(now), w.scans.full.due(now)
	if ordinary || full {
		w.refreshSources()

		if full || w.periodicReconciliation() {
			w.dirty = true
		}

		w.fullScanRequested = w.fullScanRequested || full
	}

	w.updateScanSchedule()
}

func (w *worker) updateScanSchedule() {
	if w.scans != nil {
		w.status.Scan.Next = w.scans.ordinary.next
		w.status.Scan.NextFull = w.scans.full.next
		w.status.Scan.Skipped = w.scans.ordinary.skipped + w.scans.full.skipped
		w.status.Scan.SkippedApproximate = w.scans.ordinary.approximate || w.scans.full.approximate
	}
}

func (w *worker) scheduledPolling() bool {
	return w.cfg.Watch.Backend == event.Polling &&
		(w.cfg.Watch.Reconcile.Schedule != "" || w.cfg.Integrity.Hash.FullScanSchedule != "")
}

func (w *worker) reconcile(ctx context.Context, message string) error {
	w.status.Scan.Started = time.Now()
	w.status.Scan.Result = "running"
	w.status.Scan.Full = false
	w.status.Scan.Observed, w.status.Scan.Hashed = 0, 0
	w.saveStatus(ctx)

	err := w.reconcileRun(ctx, message)
	w.status.Scan.Finished = time.Now()
	w.status.Scan.Duration = w.status.Scan.Finished.Sub(w.status.Scan.Started)

	w.status.Scan.Result = "complete"
	switch {
	case stoppedBy(ctx, err):
		w.status.Scan.Result = "interrupted"
	case err != nil:
		w.status.Scan.Result = "failed"
	case len(w.unstable) > 0:
		w.status.Scan.Result = "partial"
	default:
		w.fullScanRequested = false
	}

	if w.scans != nil {
		w.scans.ordinary.advance(w.status.Scan.Finished, false)

		if w.status.Scan.Full {
			w.scans.full.advance(w.status.Scan.Finished, false)
		}

		w.updateScanSchedule()
	}

	w.saveStatus(ctx)

	return err
}

// Cron polling retries at a configured slot rather than repeatedly walking a
// large tree outside its window. Explicit administrative work remains explicit.
func (w *worker) deferScheduledFailure() {
	if w.scheduledPolling() && !w.forceCommit {
		w.dirty, w.fullScanRequested = false, false
		w.retryAt = time.Time{}
	}
}

// Keep the configuration dependency local to scheduling rather than duplicating
// the scanner's policy or catalogue logic.
func fullScanDue(hash config.Hash, last int64, now time.Time) bool {
	return last == 0 || (hash.FullScanSchedule == "" && now.Sub(time.Unix(0, last)) >= hash.FullScanInterval)
}
