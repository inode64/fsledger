package fanotify

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
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

func (w *Watcher) handleFID(data []byte, pid int, pidfd *os.File, mask uint64) error {
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

	hasOld, hasNew := renameRecords(info)
	w.updateDirectoryCoverage(path, old, mask, hasOld, hasNew)

	if w.matcher.Match(path) {
		if old == "" || w.matcher.Match(old) {
			resource.OptionalFile(pidfd)

			return nil
		}

		path, old = old, ""
	}

	if old != "" && w.matcher.Match(old) {
		old = ""
	}

	w.queue.Send(event.Raw{
		Path: path, OldPath: old, PID: pid, PIDFD: pidfd,
		Backend: event.Fanotify, Operation: operation(mask), Time: time.Now(),
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

func renameRecords(info []pathInfo) (bool, bool) {
	var oldRecord, newRecord bool

	for _, value := range info {
		oldRecord = oldRecord || value.kind == unix.FAN_EVENT_INFO_TYPE_OLD_DFID_NAME
		newRecord = newRecord || value.kind == unix.FAN_EVENT_INFO_TYPE_NEW_DFID_NAME
	}

	return oldRecord, newRecord
}

func (w *Watcher) updateDirectoryCoverage(path, old string, mask uint64, hasOld, hasNew bool) {
	if mask&unix.FAN_ONDIR == 0 {
		return
	}

	pathSelected := pathutil.Contains(w.path, path) && !w.matcher.Match(path)
	oldSelected := old != "" && pathutil.Contains(w.path, old) && !w.matcher.Match(old)
	leftSelection := mask&unix.FAN_MOVED_FROM != 0 ||
		mask&unix.FAN_RENAME != 0 && (hasOld && !hasNew || oldSelected && !pathSelected)

	// The departed directory keeps only its own marks: Covers checks identities and accept drops paths outside
	// the selection, and every selected directory is still marked, so nothing was lost. Only the descendants'
	// records need a routine reconciliation, as after an unlink (WordPress moves each updated plugin into the
	// excluded upgrade-temp-backup).
	if leftSelection {
		w.transientPath()
	}

	if pathSelected && mask&(unix.FAN_CREATE|unix.FAN_MOVED_TO|unix.FAN_RENAME) != 0 {
		w.requestTree(path)
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
