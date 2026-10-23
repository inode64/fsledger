package inotify

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/mountinfo"
	"github.com/inode64/fsledger/internal/pathutil"
)

// RemountReason marks the reconciliation requested for a directory whose filesystem was mounted or unmounted.
// No watch reports either, so fanotify's directory marks cannot have reported it in inotify's place.
const RemountReason = "mount table changed"

// mountStacks holds the mounts stacked on the root and on each directory below it, in table order.
type mountStacks map[string][]mountinfo.Mount

// stackMounts keeps the mounts this watcher answers for. Excluded points have no watches, and the nested
// mounts the daemon found at startup are protected: each has a detector of its own.
func stackMounts(root string, matcher *exclude.Matcher, mounts []mountinfo.Mount) mountStacks {
	stacks := make(mountStacks)

	for _, mount := range mounts {
		if pathutil.Contains(root, mount.Point) && !matcher.Match(mount.Point) {
			stacks[mount.Point] = append(stacks[mount.Point], mount)
		}
	}

	return stacks
}

// changed lists, in order, the points mounted, unmounted or remounted since previous.
func (stacks mountStacks) changed(previous mountStacks) []string {
	var points []string

	for point, stack := range stacks {
		if !slices.Equal(stack, previous[point]) {
			points = append(points, point)
		}
	}

	for point := range previous {
		if _, found := stacks[point]; !found {
			points = append(points, point)
		}
	}

	slices.Sort(points)

	return points
}

// boundary reports a filesystem mounted on path below the root; the root's own filesystem is none.
func (stacks mountStacks) boundary(root, path string) bool {
	return path != root && stacks[path] != nil
}

// bounded reports a path on, or hidden by, a filesystem mounted below the root.
func (stacks mountStacks) bounded(root, path string) bool {
	for point := range stacks {
		if point != root && pathutil.Contains(point, path) {
			return true
		}
	}

	return false
}

// unwatched reports a filesystem mounted below the root, or a remote one on it: as for the remote mounts found
// at startup, the watches cannot see every change there.
func (stacks mountStacks) unwatched(root string) bool {
	for point, stack := range stacks {
		if point != root || slices.ContainsFunc(stack, mountinfo.Mount.Remote) {
			return true
		}
	}

	return false
}

// stopFollowing reports a failed mount follower; a stop without error is the watcher closing.
func (w *Watcher) stopFollowing(err error) {
	if err != nil {
		w.mountsStopped.Store(true)
		w.reportCoverageError(err)
	}
}

// followMounts reacts to the mounts and unmounts at or below the root, which no watch reports.
func (w *Watcher) followMounts(ctx context.Context) {
	defer close(w.mountsDone)

	stop := context.AfterFunc(ctx, w.changes.Stop)
	defer stop()

	for {
		changed, err := w.changes.Wait()
		if err != nil || !changed {
			w.stopFollowing(err)

			return
		}

		mounts, err := mountinfo.Read()
		if err != nil {
			w.stopFollowing(err)

			return
		}

		w.remount(stackMounts(w.path, w.matcher, mounts))
	}
}

// remount applies a mount table change. A filesystem mounted below the root is a boundary, like the nested mounts
// excluded at startup, but without a detector of its own: reconciliation covers it. A directory it no longer hides,
// and the root whose own filesystem changed, get their watches back from scratch: the kernel dropped those of an
// unmounted filesystem, which may come back with the same directories. Each changed point is then reconciled: its
// contents changed without a single event.
func (w *Watcher) remount(current mountStacks) {
	w.coverage.Lock()

	points := current.changed(w.mounts)
	w.mounts = current
	w.unwatchedFilesystem.Store(current.unwatched(w.path))

	for _, point := range points {
		if !current.bounded(w.path, point) {
			w.reportCoverageError(errors.Join(w.dropTree(point), w.addTree(point)))
		}
	}

	w.coverage.Unlock()

	for _, point := range points {
		w.queue.Send(
			event.Raw{Path: point, Backend: w.Name(), Time: time.Now(), Operation: event.Create, Reason: RemountReason},
		)
	}
}
