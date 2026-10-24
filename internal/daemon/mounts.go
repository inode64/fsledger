package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/mountinfo"
	"github.com/inode64/fsledger/internal/pathutil"
)

// errFilesystemMissing makes a repository wait while a recorded filesystem is not mounted.
var errFilesystemMissing = errors.New("recorded filesystem not mounted")

// waitForMounts records the mount points and fails while nothing is mounted at one of them. Nothing of the
// repository is analysed meanwhile: the tree of a missing filesystem would read as deleted. The failure
// takes the usual path, an error notice and retries, and the recovery follows once it is mounted again.
func (w *worker) waitForMounts(ctx context.Context) error {
	w.recordMounts(ctx)

	if len(w.status.UnmountedPaths) == 0 {
		return nil
	}

	return fmt.Errorf("%w at %s; the repository waits for it", errFilesystemMissing,
		strings.Join(w.status.UnmountedPaths, ", "))
}

// recordMounts refreshes the mount points that hold a root or are mounted inside one and are not excluded,
// unless their filesystem is volatile or a directory bound onto itself. A point stays recorded while it
// relates to a root, even unmounted; a nested one whose directory is gone as well was retired with its
// volume. The catalog keeps the points across restarts.
func (w *worker) recordMounts(ctx context.Context) {
	table, err := w.readMounts()
	if err != nil {
		w.logger.Warn("mount discovery failed; keeping the recorded mount points", "error", err)

		return
	}

	recorded := w.mergeMounts(table)
	unmounted := mountinfo.Unmounted(recorded, table)

	for _, point := range unmounted {
		if !slices.Contains(w.status.UnmountedPaths, point) {
			w.logger.Warn("recorded filesystem not mounted; the repository waits for it", "path", point)
		}
	}

	for _, point := range w.status.UnmountedPaths {
		if !slices.Contains(unmounted, point) {
			w.logger.Info("recorded filesystem mounted again; the repository resumes", "path", point)
		}
	}

	waiting := w.status.UnmountedPaths
	w.mounts, w.status.UnmountedPaths = recorded, unmounted
	w.rebindMounted(table, waiting)

	err = w.catalog.RememberMounts(ctx, recorded)
	if err != nil {
		w.logger.Warn("cannot record mount points", "error", err)
	}

	if w.mirror != nil {
		w.mirror.RememberMounts(recorded)
	}
}

// rebindMounted restarts the watchers of a root once a recorded filesystem is mounted over it, as when the
// repository waited for it or it appears over the directory they watch: they still follow what was there, and
// a pinned empty directory would read as a replaced filesystem. Mount IDs are reused, so a mount that differs
// from the one they started with is only one of the signs.
func (w *worker) rebindMounted(table []mountinfo.Mount, waiting []string) {
	var mounted []string

	for _, root := range w.roots {
		holding := mountinfo.Resolve(root, table)
		if len(holding) == 0 || !slices.Contains(w.mounts, holding[0].Point) ||
			slices.Contains(w.status.UnmountedPaths, holding[0].Point) || slices.Contains(w.returnedRoots, root) {
			continue
		}

		watched := mountinfo.Resolve(root, w.watchedMounts)
		if slices.Contains(waiting, holding[0].Point) || len(watched) > 0 && watched[0] != holding[0] {
			mounted = append(mounted, root)
		}
	}

	if len(mounted) == 0 {
		return
	}

	w.logger.Info("recorded filesystem mounted at a source; restarting its watchers", "paths", mounted)
	w.rebindRoots(mounted)

	if !w.sourcesPending {
		w.nextRoots = slices.Clone(w.roots)
	}

	w.sourcesPending, w.dirty = true, true
}

// readMounts reads the mount table once the recorded points it shows unmounted were reached, as a walk would:
// that mounts again an automounted filesystem whose idle mount expired.
func (w *worker) readMounts() ([]mountinfo.Mount, error) {
	table, err := mountinfo.Read()
	if err != nil {
		return nil, err
	}

	reached := false

	for _, point := range mountinfo.Unmounted(w.mounts, table) {
		if !remoteMount(point, table) && integrity.ReachMount(point) == nil {
			reached = true
		}
	}

	if !reached {
		return table, nil
	}

	return mountinfo.Read()
}

// remoteMount reports a path on a remote filesystem. Nothing there is probed: an unreachable server would
// block the worker.
func remoteMount(path string, table []mountinfo.Mount) bool {
	holding := mountinfo.Resolve(path, table)

	return len(holding) > 0 && holding[0].Remote()
}

// restoreMounts takes the points the catalog recorded before a restart, before anything walks a root: a
// source found on an empty mount point waits instead of being recorded as an empty tree.
func (w *worker) restoreMounts() {
	w.mounts = w.catalog.Mounts()

	if w.mirror != nil {
		w.mirror.RememberMounts(w.mounts)
	}
}

// lostMounts lists the recorded mount points that nothing is mounted at now.
func (w *worker) lostMounts() []string {
	if len(w.mounts) == 0 {
		return nil
	}

	table, err := mountinfo.Read()
	if err != nil {
		return slices.Clone(w.mounts)
	}

	return mountinfo.Unmounted(w.mounts, table)
}

// mergeMounts keeps the points that still relate to a root and were not retired, and adds the recordable
// mounts now holding a root or inside one.
func (w *worker) mergeMounts(table []mountinfo.Mount) []string {
	recorded := slices.DeleteFunc(slices.Clone(w.mounts), func(point string) bool {
		return !mountinfo.Related(point, w.roots, w.matcher.Match) || w.retired(point, table)
	})

	for _, root := range w.roots {
		for _, mount := range mountinfo.Resolve(root, table) {
			if mount.Point != "/" && !slices.Contains(recorded, mount.Point) &&
				mountinfo.Related(mount.Point, w.roots, w.matcher.Match) &&
				mountinfo.Recordable(mount, table) {
				recorded = append(recorded, mount.Point)
			}
		}
	}

	slices.Sort(recorded)

	return recorded
}

// retired reports an unmounted nested point whose directory is gone as well, as when a volume is
// decommissioned. A point holding a root is never retired, nor one inside another unmounted point, whose
// directory went with that filesystem, nor one on a remote filesystem.
func (w *worker) retired(point string, table []mountinfo.Mount) bool {
	if mountinfo.Mounted(point, table) || slices.ContainsFunc(w.roots, func(root string) bool {
		return pathutil.Contains(point, root)
	}) {
		return false
	}

	for _, other := range w.mounts {
		if other != point && pathutil.Contains(other, point) && !mountinfo.Mounted(other, table) {
			return false
		}
	}

	if remoteMount(point, table) {
		return false
	}

	_, err := os.Lstat(point)

	return integrity.Missing(err)
}
