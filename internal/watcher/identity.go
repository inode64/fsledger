package watcher

import (
	"errors"
	"time"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
)

// DirectoryIdentity tells directories apart beyond their names. The birth time separates one recreated with
// the inode number of a deleted one, which ext4 and XFS reuse at once. It is zero where the filesystem or
// the kernel provides none (ext4 with small inodes, NFSv3, kernels or sandboxes without statx), which keeps
// that reuse undetectable there; overlayfs changes it when a directory is copied up.
type DirectoryIdentity struct {
	Device uint64
	Inode  uint64
	Birth  int64
}

// IdentifyDescriptor describes an open directory; anything else is an error.
func IdentifyDescriptor(directory int) (DirectoryIdentity, error) {
	return identify(directory, "", unix.AT_EMPTY_PATH)
}

// IdentifyPath describes the directory path names, without following a final symlink.
func IdentifyPath(path string) (DirectoryIdentity, error) {
	return identify(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW)
}

func identify(directory int, path string, flags int) (DirectoryIdentity, error) {
	var info unix.Statx_t

	err := unix.Statx(directory, path, flags, unix.STATX_TYPE|unix.STATX_INO|unix.STATX_BTIME, &info)
	for errors.Is(err, unix.EINTR) {
		err = unix.Statx(directory, path, flags, unix.STATX_TYPE|unix.STATX_INO|unix.STATX_BTIME, &info)
	}

	// Without statx, as on old kernels or under a seccomp filter, the identity has no birth time.
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EPERM) {
		return identifyStat(directory, path, flags)
	}

	if err != nil {
		return DirectoryIdentity{}, fault.Wrap("identify directory", err)
	}

	if info.Mode&unix.S_IFMT != unix.S_IFDIR {
		return DirectoryIdentity{}, fault.Wrap("identify directory", unix.ENOTDIR)
	}

	identity := DirectoryIdentity{Device: unix.Mkdev(info.Dev_major, info.Dev_minor), Inode: info.Ino, Birth: 0}
	if info.Mask&unix.STATX_BTIME != 0 {
		identity.Birth = time.Unix(info.Btime.Sec, int64(info.Btime.Nsec)).UnixNano()
	}

	return identity, nil
}

func identifyStat(directory int, path string, flags int) (DirectoryIdentity, error) {
	var stat unix.Stat_t

	err := unix.Fstatat(directory, path, &stat, flags)
	if err != nil {
		return DirectoryIdentity{}, fault.Wrap("identify directory", err)
	}

	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return DirectoryIdentity{}, fault.Wrap("identify directory", unix.ENOTDIR)
	}

	return DirectoryIdentity{Device: stat.Dev, Inode: stat.Ino, Birth: 0}, nil
}
