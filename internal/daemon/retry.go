package daemon

import (
	"context"
	"errors"
	"time"
)

const maximumReconcileBackoff = time.Minute

func (w *worker) flushReconciliation(ctx context.Context, now time.Time) {
	if !w.dirty || now.Before(w.retryAt) || w.manager.HasPending() {
		return
	}

	// A filesystem mounted at a source since the last attempt restarts its watchers before the walk.
	w.recordMounts(ctx)
	w.applySources(ctx)

	err := w.reconcile(ctx, reconciliationMessage("overflow or retry"))
	if err != nil {
		if !w.interruptedByStop(ctx, err) {
			w.failedReconciliation(err)
		}

		return
	}

	err = w.completeFlushRequest()
	if err != nil {
		w.failedReconciliation(err)

		return
	}

	w.retryAt, w.retryDelay = time.Time{}, 0
	w.dirty = false
	w.contaminated = false
}

// stoppedBy reports an operation that failed because shutdown cancelled its context.
func stoppedBy(operation context.Context, err error) bool {
	return err != nil && operation.Err() != nil &&
		(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}

// interruptedByStop reports, and records, an operation that shutdown cut short. That is no failure to
// warn about or retry: the next startup reconciliation resumes the work.
func (w *worker) interruptedByStop(operation context.Context, err error) bool {
	if !stoppedBy(operation, err) {
		return false
	}

	w.interrupted = true
	w.dirty, w.contaminated = true, true

	return true
}

func (w *worker) failedReconciliation(err error) {
	defer w.deferScheduledFailure()

	w.dirty, w.contaminated = true, true
	if w.sourceRemoved(err) {
		return
	}

	w.warn(err.Error())

	w.retryDelay = doubledDelay(w.retryDelay, time.Second, maximumReconcileBackoff)
	w.retryAt = time.Now().Add(w.retryDelay)
}

// doubledDelay doubles a retry delay, starting at initial and capped at limit unless initial exceeds it.
func doubledDelay(current, initial, limit time.Duration) time.Duration {
	return max(initial, min(current+current, limit))
}

// Normal operations get a bounded grace period when shutdown begins. Explicit
// deadlines passed to commit/reconcile (including the final drain) remain effective.
func operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	stop := context.AfterFunc(parent, func() {
		timer := time.AfterFunc(gitOperationTimeout, cancel)

		<-ctx.Done()
		timer.Stop()
	})

	return ctx, func() { stop(); cancel() }
}
