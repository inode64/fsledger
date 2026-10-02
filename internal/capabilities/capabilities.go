// Package capabilities probes actual kernel calls and per-path marks.
package capabilities

import (
	"github.com/inode64/fsledger/internal/filehandle"
	"github.com/inode64/fsledger/internal/resource"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"
)

// Support distinguishes a failed syscall from an inferred kernel-version claim.
type Support struct {
	Reason    string `json:"reason,omitempty"`
	Available bool   `json:"available"`
}

// Capabilities describes independently tested fanotify modes and inotify coverage.
type Capabilities struct {
	Fanotify         Support `json:"fanotify"`
	FanotifyFID      Support `json:"fanotify_fid"`
	FanotifyDFIDName Support `json:"fanotify_dfid_name"`
	FanotifyPIDFD    Support `json:"fanotify_pidfd"`
	Inotify          Support `json:"inotify"`
	HandleResolution Support `json:"handle_resolution"`
}

func fan(path string, flags uint) Support {
	fd, err := unix.FanotifyInit(
		unix.FAN_CLASS_NOTIF|unix.FAN_CLOEXEC|unix.FAN_NONBLOCK|flags,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_LARGEFILE,
	)
	if err != nil {
		return Support{Reason: err.Error()}
	}
	defer resource.FD(fd)

	mask := uint64(unix.FAN_CLOSE_WRITE | unix.FAN_EVENT_ON_CHILD)
	if flags&unix.FAN_REPORT_FID != 0 || flags&unix.FAN_REPORT_DFID_NAME != 0 {
		mask |= unix.FAN_CREATE | unix.FAN_DELETE | unix.FAN_ONDIR
	}

	err = unix.FanotifyMark(fd, unix.FAN_MARK_ADD, mask, unix.AT_FDCWD, path)
	if err != nil {
		return Support{Reason: err.Error()}
	}

	return Support{Available: true}
}

// Probe creates and removes real marks; it never writes source files.
func Probe(path string) Capabilities {
	// An absent path is probed as given, so each capability reports the reason itself.
	target, err := filehandle.Directory(path)
	if err != nil {
		target = path
	}

	result := Capabilities{
		HandleResolution: handleSupport(target),
		Inotify:          Support{},
		Fanotify:         fan(target, 0),
		FanotifyFID:      fan(target, unix.FAN_REPORT_FID),
		FanotifyDFIDName: fan(target, unix.FAN_REPORT_DFID_NAME),
		FanotifyPIDFD:    fan(target, unix.FAN_REPORT_PIDFD),
	}

	detector, err := fsnotify.NewWatcher()
	if err == nil {
		err = detector.Add(target)

		closeErr := detector.Close()
		if err == nil {
			err = closeErr
		}
	}

	if err != nil {
		result.Inotify.Reason = err.Error()
	} else {
		result.Inotify.Available = true
	}

	return result
}

func handleSupport(path string) Support {
	resolver, err := filehandle.Open(path)
	if err != nil {
		return Support{Reason: err.Error()}
	}

	resource.Close(resolver)

	return Support{Available: true}
}
