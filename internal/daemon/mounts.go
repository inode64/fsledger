package daemon

import (
	"context"
	"os"
	"slices"
	"strings"

	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/mountinfo"
	"github.com/inode64/fsledger/internal/pathutil"
)

// recordMounts refreshes, before a reconciliation checks the roots, the record of each filesystem that holds
// a root or is mounted inside one and is not excluded, unless it is volatile or a directory bound onto
// itself. A record stays while it still relates to a root, even unmounted: its tree was lost with its
// filesystem, not deleted, so the catalog and the mirror keep it until the same filesystem is mounted again
// or leaves the selection. A nested one whose mount point is gone too was retired with it. The catalog keeps
// the records across restarts.
func (w *worker) recordMounts(ctx context.Context) {
	table, err := mountinfo.Read()
	if err != nil {
		w.logger.Warn("mount discovery failed; keeping the recorded mount points", "error", err)

		return
	}

	recorded := w.mergeMounts(table)

	var unmounted []string

	for _, record := range recorded {
		if !record.Mounted(table) {
			unmounted = append(unmounted, record.Point)
		}
	}

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

	kept := mountinfo.Lost(recorded, w.roots, table)
	w.restoreReturned(pathutil.Outside(w.kept, kept))
	w.mounts, w.kept, w.status.UnmountedPaths = recorded, kept, unmounted

	err = w.catalog.RememberMounts(ctx, recorded)
	if err != nil {
		w.logger.Warn("cannot record mount points", "error", err)
	}

	w.shareMounts()
}

// restoreReturned takes back what a filesystem mounted again holds: a root re-pins its filesystem and
// drops its copy signatures, and a reconciliation verifies the tree soon.
func (w *worker) restoreReturned(returned []string) {
	if len(returned) == 0 {
		return
	}

	roots := slices.DeleteFunc(
		slices.Clone(returned),
		func(path string) bool { return !slices.Contains(w.roots, path) },
	)
	w.scanner.ForgetRoots(roots)

	if w.mirror != nil {
		w.mirror.RebindSources(roots)
	}

	w.dirty = true
}

// liveRoots are the roots that are not set aside while their filesystem is not mounted.
func (w *worker) liveRoots() []string { return pathutil.Outside(w.roots, w.kept) }

// lostMounts lists the recorded mount points whose filesystem is not mounted now.
func (w *worker) lostMounts() []string {
	if len(w.mounts) == 0 {
		return nil
	}

	table, err := mountinfo.Read()
	if err != nil {
		return mountinfo.Points(w.mounts)
	}

	var lost []string

	for _, record := range w.mounts {
		if !record.Mounted(table) {
			lost = append(lost, record.Point)
		}
	}

	return lost
}

// mergeMounts keeps the records that still relate to a root and were not retired, and adds the recordable
// filesystems now holding a root or mounted inside one. A point keeps its first record: another filesystem
// mounted there later is not the one recorded.
func (w *worker) mergeMounts(table []mountinfo.Mount) []mountinfo.Record {
	recorded := slices.DeleteFunc(slices.Clone(w.mounts), func(record mountinfo.Record) bool {
		return !w.relatedMount(record.Point) || w.retired(record, table)
	})

	for _, root := range w.roots {
		for _, mount := range mountinfo.Resolve(root, table) {
			if mount.Point == "/" || !w.relatedMount(mount.Point) || !mountinfo.Recordable(mount, table) ||
				slices.Contains(mountinfo.Points(recorded), mount.Point) {
				continue
			}

			recorded = append(recorded, mountinfo.Describe(mount))
		}
	}

	slices.SortFunc(recorded, func(a, b mountinfo.Record) int { return strings.Compare(a.Point, b.Point) })

	return recorded
}

// retired reports an unmounted nested filesystem whose mount point is gone as well, as when a volume is
// decommissioned: nothing will be mounted there again. A point that a lost filesystem held is not retired,
// since its directory went with that filesystem.
func (w *worker) retired(record mountinfo.Record, table []mountinfo.Mount) bool {
	if record.Mounted(table) || slices.ContainsFunc(w.roots, func(root string) bool {
		return pathutil.Contains(record.Point, root)
	}) {
		return false
	}

	for _, other := range w.mounts {
		if other.Point != record.Point && pathutil.Contains(other.Point, record.Point) && !other.Mounted(table) {
			return false
		}
	}

	_, err := os.Lstat(record.Point)

	return integrity.Missing(err)
}

// restoreMounts takes the records the catalog kept before a restart, before anything walks a root: a source
// found on an empty mount point is set aside rather than recorded as an empty tree.
func (w *worker) restoreMounts() {
	w.mounts = w.catalog.Mounts()
	w.shareMounts()
}

// shareMounts gives the records to the mirror; the catalog keeps its own.
func (w *worker) shareMounts() {
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
