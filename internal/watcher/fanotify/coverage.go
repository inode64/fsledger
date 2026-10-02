package fanotify

import (
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
	"github.com/inode64/fsledger/internal/watcher"
)

type directoryIdentity struct{ device, inode uint64 }

// Covers reports verified directory-entry coverage in the full FID/name mode.
// Recreated directories must match the marked inode before companion events can be suppressed.
func (w *Watcher) Covers(path string) bool {
	if w.mode != ModeDFIDName || w.coverageLost.Load() || w.queue.HasLoss() {
		return false
	}

	parent := filepath.Dir(path)

	value, found := w.marks.Load(parent)
	if !found {
		return false
	}

	identity, valid := value.(directoryIdentity)
	if !valid {
		return false
	}

	return matchesDirectory(parent, identity)
}

func (w *Watcher) mark(path string) error {
	if !w.coverageLost.Load() {
		if value, found := w.marks.Load(path); found {
			if identity, valid := value.(directoryIdentity); valid && matchesDirectory(path, identity) {
				return nil
			}
		}
	}

	directory, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fault.Wrap("open directory to mark", err)
	}
	defer resource.FD(directory)

	err = unix.FanotifyMark(w.fd, unix.FAN_MARK_ADD|unix.FAN_MARK_ONLYDIR, w.mask, directory, ".")
	if err != nil {
		return fault.Wrap("mark directory", err)
	}

	var stat unix.Stat_t

	err = unix.Fstat(directory, &stat)
	if err != nil {
		return fault.Wrap("identify marked directory", err)
	}

	w.marks.Store(path, directoryIdentity{device: stat.Dev, inode: stat.Ino})

	return nil
}

func (w *Watcher) resetMarks() error {
	if w.fd >= 0 {
		err := unix.FanotifyMark(w.fd, unix.FAN_MARK_FLUSH, 0, unix.AT_FDCWD, "")
		if err != nil {
			return fault.Wrap("flush fanotify marks", err)
		}
	}

	w.marks.Clear()

	return nil
}

func matchesDirectory(path string, identity directoryIdentity) bool {
	var stat unix.Stat_t

	err := unix.Lstat(path, &stat)

	return err == nil && stat.Mode&unix.S_IFMT == unix.S_IFDIR && identity.device == stat.Dev &&
		identity.inode == stat.Ino
}

// PollingRequired remains true while coverage is unverified or the reader has stopped.
func (w *Watcher) PollingRequired() bool {
	return watcher.PollingRequired(w.done, &w.coverageLost)
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
