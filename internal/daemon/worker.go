package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/inode64/fsledger/internal/pathutil"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"

	procattr "github.com/inode64/fsledger/internal/attribution/proc"
	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/watcher"
)

func (w *worker) loop(ctx context.Context) error {
	// Forwarding is lightweight; saturation is sticky even if the event queue is full.
	w.startForwarding(ctx)

	tick := time.NewTicker(min(w.cfg.Commit.Debounce, schedulingInterval))
	defer tick.Stop()

	statusTick := time.NewTicker(statusInterval)
	defer statusTick.Stop()

	for {
		// select picks randomly among ready cases; a due tick must not start new work after a stop request.
		if ctx.Err() != nil {
			return w.shutdown(ctx)
		}

		select {
		case <-ctx.Done():
			return w.shutdown(ctx)
		case raw := <-w.queue.Channel:
			w.accept(raw)
		case now := <-tick.C:
			w.checkScanSchedule(now)
			w.flush(ctx, now)
			w.syncRemote(ctx, now)
		case <-statusTick.C:
			requestErr := w.checkFlushRequest()
			if requestErr != nil {
				w.warn(requestErr.Error())
			}

			w.saveStatus(ctx)
		}
	}
}

func (w *worker) accept(raw event.Raw) {
	normalized, normalizeErr := event.Normalize(raw)
	if normalizeErr != nil {
		w.loseAttribution(normalizeErr.Error())

		return
	}

	raw = normalized

	paths := []string{raw.Path}
	if raw.OldPath != "" {
		paths = append(paths, raw.OldPath)
	}

	for _, path := range paths {
		selected := false

		for _, source := range w.roots {
			if pathutil.Contains(source, path) {
				selected = true

				break
			}
		}

		if !selected || w.matcher.Match(path) {
			continue
		}

		raw.Path = path
		w.manager.Add(raw)
	}
}

func (w *worker) startForwarding(ctx context.Context) {
	forwardCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	forwarders := new(sync.WaitGroup)

	for _, detector := range w.watchers {
		forwarders.Go(func() { w.transfer(forwardCtx, detector) })
	}

	w.stopForwarding = func() {
		cancel()
		forwarders.Wait()
	}
}

// stopWatching closes every detector and accepts what was already queued.
func (w *worker) stopWatching() {
	for _, detector := range w.watchers {
		err := detector.Close()
		if err != nil {
			w.warn(err.Error())
		}
	}

	w.stopForwarding()

	for {
		select {
		case raw := <-w.queue.Channel:
			w.accept(raw)
		default:
			return
		}
	}
}

func (w *worker) shutdown(ctx context.Context) error {
	w.stopping = true
	w.stopWatching()
	w.checkLosses()

	shutdown, cancel := context.WithDeadline(context.WithoutCancel(ctx), w.stopDeadline())
	defer cancel()

	// After detectors close, queued actors cannot certify the bytes now on disk.
	// One unknown reconciliation covers pending groups and the final observation gap.
	w.manager.Due(time.Now(), true)

	if !w.cfg.Watch.Reconcile.OnStop {
		return w.finishShutdown(ctx, shutdown, nil)
	}

	// Repeating a full scan that shutdown just interrupted would only delay the stop.
	if w.interrupted && w.cfg.Watch.Reconcile.OnStart {
		w.logger.Info("shutdown reconciliation skipped; startup reconciliation will resume the interrupted work")

		return w.finishShutdown(ctx, shutdown, nil)
	}

	// Final full scan also covers kernel events queued at the moment of shutdown.
	err := w.reconcile(shutdown, reconciliationMessage("shutdown"))

	return w.finishShutdown(ctx, shutdown, err)
}

// stopDeadline measures the shutdown grace from the stop request, which may have waited for an operation.
func (w *worker) stopDeadline() time.Time {
	requested := w.stopRequested.Load()
	if requested == 0 {
		return time.Now().Add(shutdownGrace)
	}

	return time.Unix(0, requested).Add(shutdownGrace)
}

func (w *worker) flush(ctx context.Context, now time.Time) {
	ctx, cancel := operationContext(ctx)
	defer cancel()

	w.announceError(ctx)
	w.enrichAudit(ctx)

	w.checkLosses()

	if len(w.unstable) > 0 && !now.Before(w.unstableRetry) && !w.scheduledPolling() {
		w.dirty = true
	}

	for _, group := range w.manager.Due(now, w.dirty) {
		w.checkLosses()

		if w.contaminated {
			break
		}

		err := w.commit(ctx, group)
		if err != nil {
			if !w.interruptedByStop(ctx, err) && !w.sourceRemoved(err) && !w.contaminated {
				w.warn(err.Error())
			}

			w.contaminated = true
			w.dirty = true
		}
	}

	w.flushReconciliation(ctx, now)
}

func (w *worker) transfer(ctx context.Context, detector watcher.Watcher) {
	for {
		select {
		case <-ctx.Done():
			return
		case raw := <-detector.Events():
			if w.nativeCovers(raw) {
				resource.OptionalFile(raw.PIDFD)

				continue
			}

			raw = w.resolveProcess(ctx, raw)

			w.queue.Send(raw)
		}
	}
}

func (w *worker) resolveProcess(ctx context.Context, raw event.Raw) event.Raw {
	if raw.PID > 0 {
		raw.Actor = (procattr.Provider{}).Resolve(ctx, raw)
		if !raw.Actor.Known {
			w.logger.Debug("loss of attribution", "pid", raw.PID, "path", raw.Path,
				"reason", raw.Actor.UnavailableReason)
		}
	}

	if raw.PIDFD != nil {
		resource.Close(raw.PIDFD)
		raw.PIDFD = nil
	}

	return raw
}

func (w *worker) checkLosses() {
	if w.queue.Dirty() {
		w.loseAttribution(lossMessage("event loss or watcher coverage loss", w.queue.LossReason()))
	}

	for _, detector := range w.watchers {
		if requester, ok := detector.(interface{ ReconciliationPending() bool }); ok &&
			requester.ReconciliationPending() {
			w.dirty = true
		}

		if detector.Dirty() {
			reason := ""
			if source, ok := detector.(interface{ LossReason() string }); ok {
				reason = source.LossReason()
			}

			w.loseAttribution(lossMessage(detector.Name()+" event loss or coverage loss", reason))
		}
	}
}

// lossMessage names the cause when the detector reported one, such as a kernel queue overflow.
func lossMessage(summary, reason string) string {
	if reason != "" {
		summary += " (" + reason + ")"
	}

	return summary + "; reconciliation required"
}

// Loss invalidates actors already queued: their paths may now contain unobserved bytes.
func (w *worker) loseAttribution(reason string) {
	if !w.contaminated {
		w.warn(reason)
	}

	w.dirty, w.contaminated = true, true
}

func (w *worker) finishShutdown(ctx, shutdown context.Context, err error) error {
	if err != nil && ctx.Err() != nil && shutdown.Err() != nil &&
		(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		w.warn("shutdown grace period exhausted; startup reconciliation will resume unfinished work")

		err = nil
	}

	w.status.Running = false
	w.saveStatus(ctx)

	return fault.Wrap("daemon operation", err)
}

func (w *worker) periodicReconciliation() bool {
	if w.cfg.Watch.Reconcile.Enabled || w.forceReconcile {
		return true
	}

	for _, detector := range w.watchers {
		if coverage, ok := detector.(interface{ PollingRequired() bool }); ok && coverage.PollingRequired() {
			return true
		}
	}

	return false
}
