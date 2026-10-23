package fanotify

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/pathindex"
	"github.com/inode64/fsledger/internal/pathutil"
	"github.com/inode64/fsledger/internal/resource"
	"github.com/inode64/fsledger/internal/watcher"
)

type directoryIdentity struct{ device, inode uint64 }

// errOtherFilesystem refuses a mark on a filesystem mounted below the root after Start (or a btrfs subvolume).
// Its events would carry an fsid the root's handle anchor cannot resolve, or the kernel rejects the mark
// (no FID support, mixed fsids); either way every coverage refresh would end in another loss.
var errOtherFilesystem = errors.New("directory is on another filesystem than the watched root")

// Covers reports verified directory-entry coverage in the full FID/name mode.
// Recreated directories must match the marked inode before companion events can be suppressed.
func (w *Watcher) Covers(path string) bool {
	if w.mode != ModeDFIDName || w.coverageLost.Load() || w.queue.HasLoss() {
		return false
	}

	parent := filepath.Dir(path)

	identity, found := w.markedIdentity(parent)

	return found && matchesDirectory(parent, identity)
}

func (w *Watcher) markedIdentity(path string) (directoryIdentity, bool) {
	w.marksMutex.Lock()
	defer w.marksMutex.Unlock()

	return w.marks.Get(path)
}

func (w *Watcher) rememberMark(path string, identity directoryIdentity) {
	w.marksMutex.Lock()
	defer w.marksMutex.Unlock()

	w.marks.Set(path, identity)
}

func (w *Watcher) clearMarks() {
	w.marksMutex.Lock()
	defer w.marksMutex.Unlock()

	w.marks = pathindex.Index[directoryIdentity]{}
}

func (w *Watcher) mark(path string) error {
	if !w.coverageLost.Load() {
		if identity, found := w.markedIdentity(path); found && matchesDirectory(path, identity) {
			return nil
		}
	}

	directory, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fault.Wrap("open directory to mark", err)
	}
	defer resource.FD(directory)

	var stat unix.Stat_t

	err = unix.Fstat(directory, &stat)
	if err != nil {
		return fault.Wrap("identify directory to mark", err)
	}

	// The descriptor about to be marked is checked, so a mount racing the walk cannot slip in.
	if stat.Dev != w.device {
		return errOtherFilesystem
	}

	err = unix.FanotifyMark(w.fd, unix.FAN_MARK_ADD|unix.FAN_MARK_ONLYDIR, w.mask, directory, ".")
	if err != nil {
		return fault.Wrap("mark directory", err)
	}

	w.rememberMark(path, directoryIdentity{device: stat.Dev, inode: stat.Ino})

	return nil
}

// forgetMarks drops the identities remembered for a directory that left its path and for its subtree. The kernel
// destroys a deleted directory's marks with its inode, and a directory recreated there may reuse the inode number
// (ext4, xfs): the stale identity would let mark skip fanotify_mark and Covers suppress companion events of an
// unmarked directory. The subtree goes too: a moved directory takes it along, and the child deletions of a removed
// one name a parent that may no longer resolve.
func (w *Watcher) forgetMarks(path string) {
	if path == "" || !pathutil.Contains(w.path, path) {
		return
	}

	w.marksMutex.Lock()
	defer w.marksMutex.Unlock()

	w.marks.DeleteSubtree(path)
}

func (w *Watcher) resetMarks() error {
	if w.fd >= 0 {
		err := unix.FanotifyMark(w.fd, unix.FAN_MARK_FLUSH, 0, unix.AT_FDCWD, "")
		if err != nil {
			return fault.Wrap("flush fanotify marks", err)
		}
	}

	w.clearMarks()

	return nil
}

func matchesDirectory(path string, identity directoryIdentity) bool {
	var stat unix.Stat_t

	err := unix.Lstat(path, &stat)

	return err == nil && stat.Mode&unix.S_IFMT == unix.S_IFDIR && identity.device == stat.Dev &&
		identity.inode == stat.Ino
}

// PollingRequired remains true while coverage is unverified, the reader has stopped, or directories below
// the root are left unmarked, on another filesystem or beyond the mark limit: as for a nested mount without a
// usable detector at startup, reconciliation covers them.
func (w *Watcher) PollingRequired() bool {
	return w.unmarked.Load() || watcher.PollingRequired(w.done, &w.coverageLost)
}

func (w *Watcher) needsCoverageRefresh(now, lastSweep time.Time) bool {
	if w.mode != ModeDFIDName || w.coverageLost.Load() || now.Sub(lastSweep) >= coverageSweepInterval {
		return true
	}

	info, err := os.Lstat(w.path)
	if err != nil {
		return true
	}

	path := w.path
	if info.IsDir() {
		path = filepath.Join(path, ".fsledger-coverage")
	}

	return !w.Covers(path)
}
