package daemon

import (
	"context"
	"sync"

	"github.com/inode64/fsledger/internal/pathutil"

	"github.com/inode64/fsledger/internal/event"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/exclude"
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

func (w *worker) nativeCovers(raw event.Raw) bool {
	if raw.Backend != event.Inotify {
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
	if mount.Remote() {
		w.forceReconcile = true
		w.warn("remote filesystem requires reconciliation: " + path)
	}

	label := w.tryFanotify(ctx, path, matcher)

	var selected watcher.Watcher

	if w.cfg.Watch.Backend != event.Polling {
		candidate := w.tryInotify(ctx, path, matcher)
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

func (w *worker) tryFanotify(
	ctx context.Context,
	path string,
	matcher *exclude.Matcher,
) string {
	if w.cfg.Watch.Backend != config.BackendAuto && w.cfg.Watch.Backend != event.Fanotify {
		return ""
	}

	candidate, err := fanotify.New(path, matcher)
	if err == nil {
		err = candidate.Start(ctx)
		if err == nil {
			w.watchers = append(w.watchers, candidate)
			w.logger.Info("fanotify mode selected", "path", path, "mode", candidate.Mode())
			w.checkPIDFDLifetime(ctx, candidate.PIDFD())

			return event.Fanotify + "+"
		}

		resource.Close(candidate)
	}

	if w.cfg.Watch.Backend == config.BackendAuto {
		w.logger.Info("fanotify unavailable; trying fallback", "path", path, "reason", err.Error())
	} else {
		w.warn("fanotify fallback: " + err.Error())
	}

	return ""
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

func (w *worker) tryInotify(ctx context.Context, path string, matcher *exclude.Matcher) *inotify.Watcher {
	candidate, err := inotify.New(path, matcher)
	if err != nil {
		w.warn("inotify init fallback: " + err.Error())

		return nil
	}

	err = candidate.Start(ctx)
	if err != nil {
		resource.Close(candidate)
		w.warn("inotify coverage fallback: " + err.Error())

		return nil
	}

	return candidate
}
