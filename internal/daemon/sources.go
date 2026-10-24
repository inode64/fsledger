package daemon

import (
	"context"
	"errors"
	"os"
	"slices"
	"sync/atomic"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/pathutil"
)

// activeRoots publishes a worker's current sources to readers on other goroutines.
type activeRoots struct{ value atomic.Pointer[[]string] }

func newActiveRoots(roots []string) *activeRoots {
	active := new(activeRoots)
	active.store(roots)

	return active
}

func (active *activeRoots) load() []string { return *active.value.Load() }

func (active *activeRoots) store(roots []string) {
	roots = slices.Clone(roots)
	active.value.Store(&roots)
}

func publishRoots(resolved map[string]config.Sources) map[string]*activeRoots {
	roots := make(map[string]*activeRoots, len(resolved))
	for name, sources := range resolved {
		roots[name] = newActiveRoots(sources.Roots)
	}

	return roots
}

// recordSources exposes what the configured paths currently mean. Absent entries are expected
// with a shared list, so only skipped matches are a problem worth an error notification.
func (w *worker) recordSources(sources config.Sources) {
	if !slices.Equal(w.status.MissingSources, sources.Missing) {
		w.logger.Info("configured sources absent", "count", len(sources.Missing))
		w.logger.Debug("configured sources absent", "paths", sources.Missing)
	}

	if !slices.Equal(w.status.SkippedSources, sources.Skipped) {
		for _, reason := range sources.Skipped {
			w.warn("source skipped: " + reason)
		}
	}

	w.status.MissingSources, w.status.SkippedSources = sources.Missing, sources.Skipped
}

// refreshSources re-reads what the configured paths select. It reports a pending change, which
// the next reconciliation applies.
func (w *worker) refreshSources() bool {
	return w.updateSources(w.cfg.ResolveRepository(w.name))
}

func (w *worker) updateSources(sources config.Sources) bool {
	resolved := sources.Roots
	next, unavailable := w.retainedSources(resolved, sources.Incomplete)
	returned := selectedRoots(w.status.UnavailableSources, resolved)

	if len(returned) > 0 {
		w.rebindRoots(returned)
	}

	w.recordSources(sources)
	w.status.UnavailableSources = unavailable

	// An empty selection is a change too, so the pending state cannot live in the slice itself.
	w.nextRoots, w.sourcesPending = nil, !slices.Equal(next, w.roots) || len(w.returnedRoots) > 0
	if w.sourcesPending {
		w.nextRoots, w.dirty = next, true
	}

	return w.sourcesPending
}

// rebindRoots forgets the identities of roots whose filesystem returned or changed. Their watchers still
// follow the previous filesystem, and a later refresh no longer sees the change, so the restart stays pending
// until applySources.
func (w *worker) rebindRoots(roots []string) {
	w.scanner.ForgetRoots(roots)

	if w.mirror != nil {
		w.mirror.RebindSources(roots)
	}

	w.returnedRoots = pathutil.CompactRoots(append(w.returnedRoots, roots...))
}

func (w *worker) retainedSources(resolved []string, incomplete bool) ([]string, []string) {
	next := slices.Clone(resolved)
	if incomplete {
		next = append(next, w.roots...)
	}

	var unavailable []string

	lost := w.lostMounts()

	for _, root := range w.roots {
		if slices.Contains(resolved, root) {
			continue
		}

		if w.lostRoot(root, lost) {
			unavailable = append(unavailable, root)
		}
	}

	return pathutil.CompactRoots(append(next, unavailable...)), unavailable
}

// lostRoot tells a source lost with its filesystem from one deleted or merely no longer selected: a root
// that still exists stopped being selected, and a missing one leaves only when it was deleted. A lost
// filesystem must never be recorded as an empty tree.
func (w *worker) lostRoot(root string, lost []string) bool {
	_, err := os.Lstat(root)

	return err != nil && (w.scanner.RootUnavailable(root) || pathutil.Within(lost, root))
}

// retainRecorded is retainedSources for startup, which has no previous roots: an incomplete
// selection keeps every tree the catalog recorded outside it, so what could not be read is not pruned,
// and a complete one keeps those whose recorded mount point is no longer mounted.
func (w *worker) retainRecorded(ctx context.Context, incomplete bool) error {
	lost := w.lostMounts()
	if !incomplete && len(lost) == 0 {
		return nil
	}

	recorded, err := w.catalog.RecordedOutside(ctx, w.roots)
	if err != nil || len(recorded) == 0 {
		return err
	}

	if !incomplete {
		recorded = slices.DeleteFunc(recorded, func(root string) bool { return !w.lostRoot(root, lost) })
		if len(recorded) == 0 {
			return nil
		}

		w.status.UnavailableSources = recorded
	}

	w.logger.Info("recorded sources retained", "count", len(recorded))
	w.logger.Debug("recorded sources retained", "paths", recorded)

	w.roots = pathutil.CompactRoots(append(slices.Clone(w.roots), recorded...))
	w.status.Paths = w.roots
	w.published.store(w.roots)

	if w.mirror != nil {
		w.mirror.SetSources(w.roots)
	}

	return nil
}

func selectedRoots(candidates, selected []string) []string {
	return slices.DeleteFunc(slices.Clone(candidates), func(root string) bool {
		return !slices.Contains(selected, root)
	})
}

// sourceRemoved recognizes an operation that failed only because a selected source was deleted.
// The pending source change then records the deletion, without an error notification.
func (w *worker) sourceRemoved(err error) bool {
	return errors.Is(err, integrity.ErrUnavailable) && w.refreshSources()
}

// applySources swaps the watched roots as a whole and reuses the startup path. Events queued for
// the previous roots lose their actor: the reconciliation that follows certifies the bytes.
func (w *worker) applySources(ctx context.Context) {
	if !w.sourcesPending {
		return
	}

	w.stopWatching()
	w.manager.Clear()
	w.logger.Info("sources changed", "previous", w.roots, "current", w.nextRoots)

	var removed []string

	if w.mirror != nil {
		removed = w.mirror.SetSources(w.nextRoots)
	} else {
		removed = slices.DeleteFunc(slices.Clone(w.roots), func(root string) bool {
			return slices.Contains(w.nextRoots, root)
		})
	}

	w.scanner.ForgetRoots(removed)

	w.roots, w.status.Paths, w.nextRoots, w.returnedRoots, w.sourcesPending = w.nextRoots, w.nextRoots, nil, nil, false
	w.published.store(w.roots)
	w.watchers, w.status.Backends, w.forceReconcile = nil, nil, false
	w.contaminated = true
	// The watchers outlive this operation: they end through Close, like the ones started at startup.
	w.startWatchers(context.WithoutCancel(ctx))
	w.startForwarding(ctx)
}
