package fanotify

import (
	"errors"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/filehandle"
	"github.com/inode64/fsledger/internal/resource"
)

// Mode names describe the representation reported in logs and diagnostics.
const (
	ModeFD       = "fd"
	ModeFID      = "fid"
	ModeDFIDName = "dfid_name"
)

// Mode describes the event representation actually installed, not just probed flags.
func (w *Watcher) Mode() string { return w.mode }

// PIDFD reports whether descriptor attribution was actually enabled for this group.
func (w *Watcher) PIDFD() bool { return w.pidfd }

func (w *Watcher) initialize() error {
	resolver, resolveErr := filehandle.Open(w.path)
	if resolveErr == nil {
		w.handles = resolver
	}

	flags := []uint{unix.FAN_REPORT_FID | unix.FAN_REPORT_DFID_NAME, unix.FAN_REPORT_FID, 0}

	var last error

	for _, mode := range flags {
		if mode != 0 && w.handles == nil {
			continue
		}

		for _, pidfd := range []uint{unix.FAN_REPORT_PIDFD, 0} {
			last = w.tryMode(mode, pidfd)
			if last == nil {
				if mode == 0 {
					// Descriptor events carry their own path; the handle anchors would stay open unused.
					w.releaseHandles()
				}

				return nil
			}
		}
	}

	w.releaseHandles()

	return fault.Wrap("initialize fanotify modes", last)
}

func (w *Watcher) tryMode(mode, pidfd uint) error {
	fd, err := unix.FanotifyInit(unix.FAN_CLASS_NOTIF|unix.FAN_CLOEXEC|unix.FAN_NONBLOCK|mode|pidfd,
		unix.O_RDONLY|unix.O_LARGEFILE|unix.O_CLOEXEC)
	if err != nil {
		return fault.Wrap("fanotify init", err)
	}

	w.fd = fd
	w.pidfd = pidfd != 0
	w.mask = notificationMask
	// Remembered marks belong to a group: a new group starts with none, or it would never be marked.
	w.marks.Clear()

	w.mode = ModeFD
	if mode != 0 {
		w.mode = ModeFID
		w.mask |= unix.FAN_ATTRIB | unix.FAN_ONDIR
	}

	if mode&unix.FAN_REPORT_NAME != 0 {
		w.mode = ModeDFIDName
		w.mask |= unix.FAN_CREATE | unix.FAN_DELETE | unix.FAN_RENAME
	}

	err = w.probeMarks()
	if errors.Is(err, unix.EINVAL) && w.mask&unix.FAN_RENAME != 0 {
		w.mask = (w.mask &^ unix.FAN_RENAME) | unix.FAN_MOVED_FROM | unix.FAN_MOVED_TO
		err = w.probeMarks()
	}

	if err != nil {
		resource.FD(fd)
		w.fd = -1
	}

	return err
}

func (w *Watcher) releaseHandles() {
	if w.handles != nil {
		resource.Close(w.handles)
		w.handles = nil
	}
}

func (w *Watcher) probeMarks() error {
	directory, err := filehandle.Directory(w.path)
	if err != nil {
		return err
	}

	return w.mark(directory)
}
