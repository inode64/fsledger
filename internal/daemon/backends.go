package daemon

import (
	"context"
	"sync"

	"github.com/inode64/fsledger/internal/pathutil"

	"github.com/inode64/fsledger/internal/event"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/mountinfo"
	"github.com/inode64/fsledger/internal/resource"
	"github.com/inode64/fsledger/internal/watcher"
	"github.com/inode64/fsledger/internal/watcher/fanotify"
	"github.com/inode64/fsledger/internal/watcher/inotify"
)

// startWatchers cannot fail: a root without a usable detector falls back to reconciliation.
func (w *worker) startWatchers(ctx context.Context) {
	mounts, err := mountinfo.Read()
	if err != nil {
		w.warn("mount discovery failed; forcing reconciliation: " + err.Error())
		w.forceReconcile = true
	}

	for _, root := range w.roots {
		w.startRoot(ctx, root, mounts)
	}
}

// nativeCovers reports an inotify event that fanotify's directory marks delivered too. No mark sees a mount
// or unmount, so the reconciliation inotify requests for a remounted directory is never one of them.
func (w *worker) nativeCovers(raw event.Raw) bool {
	if raw.Backend != event.Inotify || raw.Reason == inotify.RemountReason {
		return false
	}

	for _, detector := range w.watchers {
		candidate, ok := detector.(*fanotify.Watcher)
		if ok && candidate.Covers(raw.Path) {
			return true
		}
	}

	return false
}

func (w *worker) startRoot(ctx context.Context, root string, mounts []mountinfo.Mount) {
	boundaries := mountinfo.Resolve(root, mounts)
	if len(boundaries) == 0 {
		boundaries = []mountinfo.Mount{{Point: root, Type: event.Unknown}}
	}

	for index, mount := range boundaries {
		path := root
		if index > 0 {
			path = mount.Point
		}

		if w.matcher.Match(path) {
			continue
		}

		pathMatcher := w.boundaryMatcher(path, boundaries)

		w.startBoundary(ctx, path, mount, pathMatcher)
	}
}

func (w *worker) startBoundary(
	ctx context.Context,
	path string,
	mount mountinfo.Mount,
	matcher *exclude.Matcher,
) {
	// Changes made through another client of a remote filesystem raise no local event: a lasting property of
	// the filesystem, which doctor reports, not an error to announce and recover from on every start.
	if mount.Remote() {
		w.forceReconcile = true
		w.logger.Info("remote filesystem requires reconciliation", "path", path, "filesystem", mount.Type)
	}

	// Like the pidfd advisory, an attribute comparison the filesystem cannot provide is a lasting
	// limitation, not an error.
	for _, message := range integrity.AttributeAdvisories(w.cfg.Integrity, path, mount.Type) {
		w.logger.Warn("attribute comparison unsupported", "reason", message)
		w.advise(message)
	}

	companion := w.tryFanotify(ctx, path, matcher)

	label := ""
	if companion != nil {
		label = event.Fanotify + "+"
	}

	var selected watcher.Watcher

	if w.cfg.Watch.Backend != event.Polling {
		candidate := w.tryInotify(ctx, path, matcher, companion)
		if candidate != nil {
			selected = candidate
		}
	}

	if selected == nil {
		w.forceReconcile = true
		label += event.Polling
	} else {
		w.watchers = append(w.watchers, selected)
		label += selected.Name()
	}

	w.status.Backends = append(w.status.Backends, path+": "+label)
	w.logger.Info("backend selected", "path", path, "mount", mount.Point, "filesystem", mount.Type, "backend", label)
}

// tryFanotify starts fanotify for path, or returns nil when it is not selected or unavailable.
func (w *worker) tryFanotify(
	ctx context.Context,
	path string,
	matcher *exclude.Matcher,
) *fanotify.Watcher {
	if w.cfg.Watch.Backend != config.BackendAuto && w.cfg.Watch.Backend != event.Fanotify {
		return nil
	}

	candidate, err := fanotify.New(path, matcher)
	if err == nil {
		err = candidate.Start(ctx)
		if err == nil {
			w.watchers = append(w.watchers, candidate)
			w.logger.Info("fanotify mode selected", "path", path, "mode", candidate.Mode())
			w.checkPIDFDLifetime(ctx, candidate.PIDFD())

			return candidate
		}

		resource.Close(candidate)
	}

	if w.cfg.Watch.Backend == config.BackendAuto {
		w.logger.Info("fanotify unavailable; trying fallback", "path", path, "reason", err.Error())
	} else {
		w.warn("fanotify fallback: " + err.Error())
	}

	return nil
}

func (w *worker) checkPIDFDLifetime(ctx context.Context, enabled bool) {
	if !enabled || w.status.FanotifyPIDFDLifetime != nil || ctx.Err() != nil {
		return
	}

	result := w.probePIDFD(ctx, w.cfg.Runtime)
	if ctx.Err() == nil {
		w.recordPIDFDLifetime(result)
	}
}

type pidfdProbe func(context.Context, string) fanotify.PIDFDLifetime

// A conclusive probe describes the daemon's kernel, not an individual repository.
// Cancellation and transient failures must leave later callers free to try again.
func sharedPIDFDProbe(probe pidfdProbe) pidfdProbe {
	var (
		mutex  sync.Mutex
		result fanotify.PIDFDLifetime
	)

	return func(ctx context.Context, runtime string) fanotify.PIDFDLifetime {
		mutex.Lock()
		defer mutex.Unlock()

		if result.Checked || ctx.Err() != nil {
			return result
		}

		observed := probe(ctx, runtime)
		if observed.Checked && ctx.Err() == nil {
			result = observed
		}

		return observed
	}
}

func (w *worker) recordPIDFDLifetime(result fanotify.PIDFDLifetime) {
	w.status.FanotifyPIDFDLifetime = &result

	message := result.Warning()
	if message != "" {
		// A limitation of actor detail is not evidence of lost filesystem events.
		// Keep it visible without generating a spurious error/recovery or report gap.
		w.logger.Warn("fanotify attribution warning", "reason", message)
		w.advise(message)
	}
}

func (w *worker) boundaryMatcher(path string, boundaries []mountinfo.Mount) *exclude.Matcher {
	var nestedPaths []string

	for _, nested := range boundaries {
		if nested.Point != path && pathutil.Contains(path, nested.Point) {
			nestedPaths = append(nestedPaths, nested.Point)
		}
	}

	return w.matcher.Protect(nestedPaths...)
}

// tryInotify starts inotify for path. Its mount follower tells fanotify, when it runs for the same path, of every
// remounted directory: no fanotify mark sees a mount or an unmount.
func (w *worker) tryInotify(
	ctx context.Context,
	path string,
	matcher *exclude.Matcher,
	companion *fanotify.Watcher,
) *inotify.Watcher {
	candidate, err := inotify.New(path, matcher)
	if err != nil {
		w.warn("inotify init fallback: " + err.Error())

		return nil
	}

	if companion != nil {
		candidate.OnRemount(companion.Remount)
	}

	err = candidate.Start(ctx)
	if err != nil {
		resource.Close(candidate)
		w.warn("inotify coverage fallback: " + err.Error())

		return nil
	}

	return candidate
}
