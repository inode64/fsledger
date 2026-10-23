package watcher

import (
	"time"

	"golang.org/x/sys/unix"
)

// DirectoryIdentity tells directories apart beyond their names. The birth time separates one recreated with
// the inode number of a deleted one, which ext4 and XFS reuse at once; it is zero where unsupported.
type DirectoryIdentity struct {
	Device uint64
	Inode  uint64
	Birth  int64
}

// IdentifyDescriptor describes an open directory; false means it is not one or cannot be described.
func IdentifyDescriptor(directory int) (DirectoryIdentity, bool) {
	return identify(directory, "", unix.AT_EMPTY_PATH)
}

// IdentifyPath describes the directory path names, without following a final symlink.
func IdentifyPath(path string) (DirectoryIdentity, bool) {
	return identify(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW)
}

func identify(directory int, path string, flags int) (DirectoryIdentity, bool) {
	var info unix.Statx_t

	err := unix.Statx(directory, path, flags, unix.STATX_TYPE|unix.STATX_INO|unix.STATX_BTIME, &info)
	if err != nil || info.Mode&unix.S_IFMT != unix.S_IFDIR {
		return DirectoryIdentity{}, false
	}

	identity := DirectoryIdentity{Device: unix.Mkdev(info.Dev_major, info.Dev_minor), Inode: info.Ino, Birth: 0}
	if info.Mask&unix.STATX_BTIME != 0 {
		identity.Birth = time.Unix(info.Btime.Sec, int64(info.Btime.Nsec)).UnixNano()
	}

	return identity, true
}
