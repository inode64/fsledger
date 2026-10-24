package daemon

import (
	"context"
	"slices"

	"github.com/inode64/fsledger/internal/mountinfo"
	"github.com/inode64/fsledger/internal/pathutil"
)

// recordMounts refreshes, before a reconciliation checks the roots, the mount points that hold a root or
// lie inside one and are not excluded. A recorded point stays while it still relates to a root, even
// unmounted: its tree was lost with its filesystem, not deleted, so the catalog and the mirror keep it
// until it is mounted again or leaves the selection. The catalog keeps the set across restarts.
func (w *worker) recordMounts(ctx context.Context) {
	mounts, err := mountinfo.Read()
	if err != nil {
		w.logger.Warn("mount discovery failed; keeping the recorded mount points", "error", err)

		return
	}

	recorded := slices.DeleteFunc(slices.Clone(w.mounts), func(point string) bool { return !w.relatedMount(point) })

	for _, root := range w.roots {
		for _, mount := range mountinfo.Resolve(root, mounts) {
			if mount.Point != "/" && w.relatedMount(mount.Point) && !slices.Contains(recorded, mount.Point) {
				recorded = append(recorded, mount.Point)
			}
		}
	}

	slices.Sort(recorded)

	unmounted := slices.DeleteFunc(slices.Clone(recorded), func(point string) bool {
		return slices.ContainsFunc(mounts, func(mount mountinfo.Mount) bool { return mount.Point == point })
	})

	for _, point := range unmounted {
		if !slices.Contains(w.status.UnmountedPaths, point) {
			w.logger.Warn("filesystem no longer mounted; its recorded tree is kept", "path", point)
		}
	}

	for _, point := range w.status.UnmountedPaths {
		if !slices.Contains(unmounted, point) {
			w.logger.Info("recorded filesystem mounted again", "path", point)
		}
	}

	w.mounts, w.status.UnmountedPaths = recorded, unmounted

	err = w.catalog.RememberMounts(ctx, recorded)
	if err != nil {
		w.logger.Warn("cannot record mount points", "error", err)
	}

	w.expectMounts()
}

// restoreMounts takes the mount points the catalog recorded before a restart, before anything pins a root:
// a source found on an empty mount point is unavailable rather than an empty tree.
func (w *worker) restoreMounts() {
	w.mounts = w.catalog.Mounts()
	w.expectMounts()
}

func (w *worker) expectMounts() {
	w.scanner.ExpectMounts(w.mounts)

	if w.mirror != nil {
		w.mirror.RememberMounts(w.mounts)
	}
}

// relatedMount reports a mount point that holds a root, or lies inside one and is not excluded.
func (w *worker) relatedMount(point string) bool {
	for _, root := range w.roots {
		if pathutil.Contains(point, root) || pathutil.Contains(root, point) && !w.matcher.Match(point) {
			return true
		}
	}

	return false
}
