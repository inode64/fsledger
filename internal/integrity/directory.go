package integrity

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

// DirectoryFlags pins a directory only when the component itself is one, never through a symlink.
const DirectoryFlags = unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// Descend opens parts below the directory fd without following symlinks and consumes fd, even on failure.
// A nonzero mode creates each missing directory with it and makes the creation durable.
func Descend(fd int, parts []string, mode uint32) (int, error) {
	for _, part := range parts {
		next, err := unix.Openat(fd, part, DirectoryFlags, 0)
		if errors.Is(err, os.ErrNotExist) && mode != 0 {
			err = unix.Mkdirat(fd, part, mode)
			if err == nil {
				err = SyncDirectory(fd)
			}

			if err == nil || errors.Is(err, os.ErrExist) {
				next, err = unix.Openat(fd, part, DirectoryFlags, 0)
			}
		}

		resource.FD(fd)

		if err != nil {
			return -1, fault.Wrap("open directory "+part, err)
		}

		fd = next
	}

	return fd, nil
}

// SyncDirectory makes changes to a pinned directory's entries durable through a readable copy of it:
// fsync refuses an O_PATH descriptor.
func SyncDirectory(fd int) error {
	readable, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fault.Wrap("open directory for sync", err)
	}
	defer resource.FD(readable)

	return fault.Wrap("sync directory", unix.Fsync(readable))
}
