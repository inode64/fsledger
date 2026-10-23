package fanotify

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/filehandle"
	"github.com/inode64/fsledger/internal/pathutil"
	"github.com/inode64/fsledger/internal/resource"
)

type pathInfo struct {
	name   string
	handle filehandle.Handle
	kind   byte
}

func parseFIDs(data []byte) ([]pathInfo, error) {
	var result []pathInfo

	for len(data) > 0 {
		if len(data) < infoHeaderSize {
			return nil, fault.New("truncated FID info header")
		}

		length := int(binary.NativeEndian.Uint16(data[2:4]))
		if length < infoHeaderSize || length > len(data) {
			return nil, fault.New("invalid FID info length")
		}

		record := data[:length]
		data = data[length:]

		switch record[0] {
		case unix.FAN_EVENT_INFO_TYPE_FID, unix.FAN_EVENT_INFO_TYPE_DFID,
			unix.FAN_EVENT_INFO_TYPE_DFID_NAME, unix.FAN_EVENT_INFO_TYPE_OLD_DFID_NAME, unix.FAN_EVENT_INFO_TYPE_NEW_DFID_NAME:
			value, err := parseHandle(record)
			if err != nil {
				return nil, err
			}

			result = append(result, value)
		}
	}

	return result, nil
}

func parseHandle(record []byte) (pathInfo, error) {
	const headerSize = 20
	if len(record) < headerSize {
		return pathInfo{}, fault.New("truncated filesystem handle")
	}

	size := binary.NativeEndian.Uint32(record[12:16])
	if size == 0 || int64(size) > int64(len(record)-headerSize) {
		return pathInfo{}, fault.New("invalid filesystem handle size")
	}

	end := headerSize + int(size)
	value := pathInfo{
		kind:   record[0],
		name:   "",
		handle: filehandle.Handle{FSID: [8]byte{}, Bytes: record[headerSize:end], Type: 0},
	}
	copy(value.handle.FSID[:], record[4:12])
	//nolint:gosec // The handle type is a signed 32-bit Linux ABI field.
	value.handle.Type = int32(binary.NativeEndian.Uint32(record[16:20]))
	if value.kind == unix.FAN_EVENT_INFO_TYPE_FID || value.kind == unix.FAN_EVENT_INFO_TYPE_DFID {
		return value, nil
	}

	terminator := bytes.IndexByte(record[end:], 0)
	if terminator < 1 {
		return pathInfo{}, fault.New("missing directory entry name")
	}

	value.name = string(record[end : end+terminator])
	if value.name == ".." || strings.ContainsRune(value.name, '/') {
		return pathInfo{}, fault.New("invalid directory entry name")
	}

	return value, nil
}

func (w *Watcher) fidPaths(info []pathInfo) (string, string, error) {
	var path, old string
	// Named directory records survive deletion of the target; prefer them over FID.
	for _, value := range info {
		if value.name == "" {
			continue
		}

		parent, err := w.handles.Path(value.handle)
		if err != nil {
			return "", "", err
		}

		resolved := filepath.Join(parent, value.name)
		if value.kind == unix.FAN_EVENT_INFO_TYPE_OLD_DFID_NAME {
			old = resolved
		} else {
			path = resolved
		}
	}

	if path != "" || old != "" {
		if path == "" {
			return old, "", nil
		}

		return path, old, nil
	}

	for _, value := range info {
		if value.kind == unix.FAN_EVENT_INFO_TYPE_FID {
			resolved, err := w.handles.Path(value.handle)

			return resolved, "", err
		}
	}

	return "", "", fault.New("fanotify event has no resolvable FID")
}

func (w *Watcher) handleFID(data []byte, pid int, pidfd *os.File, reaped bool, mask uint64) error {
	info, err := parseFIDs(data)
	if err != nil {
		resource.OptionalFile(pidfd)

		return err
	}

	path, old, err := w.fidPaths(info)
	if err != nil {
		resource.OptionalFile(pidfd)

		if transientFIDError(err) {
			w.unresolvedFID(mask)

			return nil
		}

		return err
	}

	w.updateDirectoryCoverage(path, old, mask, hasOldRecord(info))

	if w.ignored(path) {
		if old == "" || w.ignored(old) {
			resource.OptionalFile(pidfd)

			return nil
		}

		path, old = old, ""
	}

	if old != "" && w.ignored(old) {
		old = ""
	}

	w.queue.Send(event.Raw{
		Path: path, OldPath: old, PID: pid, PIDFD: pidfd, Reaped: reaped,
		Backend: event.Fanotify, Operation: operation(mask), Time: time.Now(), SinceBoot: sinceBoot(),
	})

	return nil
}

func transientFIDError(err error) bool {
	return errors.Is(err, unix.ESTALE) || errors.Is(err, os.ErrNotExist) || errors.Is(err, filehandle.ErrDeleted)
}

func (w *Watcher) unresolvedFID(mask uint64) {
	if mask&unix.FAN_ONDIR != 0 && mask&(unix.FAN_RENAME|unix.FAN_MOVED_FROM) != 0 {
		w.loss("fanotify could not resolve moved directory coverage")

		return
	}

	w.transientPath()
}

// hasOldRecord reports a rename's old name, which a one-sided rename out of the marked directories carries alone.
func hasOldRecord(info []pathInfo) bool {
	return slices.ContainsFunc(
		info,
		func(value pathInfo) bool { return value.kind == unix.FAN_EVENT_INFO_TYPE_OLD_DFID_NAME },
	)
}

func (w *Watcher) updateDirectoryCoverage(path, old string, mask uint64, hasOld bool) {
	if mask&unix.FAN_ONDIR == 0 {
		return
	}

	w.forgetMarks(departedDirectory(path, old, mask, hasOld))

	// A directory leaving the selection keeps only its own marks: Covers checks identities and accept drops
	// paths outside the selection, and every selected directory is still marked, so nothing was lost. The
	// event names its departed path, and observing that missing path removes its descendants' records and
	// mirror copies: no full reconciliation is needed (WordPress moves each updated plugin into the excluded
	// upgrade-temp-backup).
	pathSelected := pathutil.Contains(w.path, path) && !w.matcher.Match(path)
	if pathSelected && mask&(unix.FAN_CREATE|unix.FAN_MOVED_TO|unix.FAN_RENAME) != 0 {
		w.requestTree(path)
	}
}

// departedDirectory names the directory entry an event removed: a deleted or moved-away name, or a rename's old
// name. It is empty when the event only created a name.
func departedDirectory(path, old string, mask uint64, hasOld bool) string {
	switch {
	case old != "":
		return old
	case hasOld || mask&(unix.FAN_DELETE|unix.FAN_MOVED_FROM) != 0:
		return path
	default:
		return ""
	}
}

func operation(mask uint64) string {
	switch {
	case mask&(unix.FAN_DELETE|unix.FAN_MOVED_FROM) != 0:
		return event.Remove
	case mask&unix.FAN_RENAME != 0:
		return event.Rename
	case mask&(unix.FAN_CREATE|unix.FAN_MOVED_TO) != 0:
		return event.Create
	case mask&unix.FAN_ATTRIB != 0:
		return event.Attrib
	default:
		return event.Write
	}
}
