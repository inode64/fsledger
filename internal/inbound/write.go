package inbound

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

func writeTemporary(parent int, name, temp, mode string, actual observed, data []byte) error {
	if mode == symlink {
		err := unix.Symlinkat(string(data), parent, temp)
		if err == nil && actual.mode != absent {
			err = unix.Fchownat(parent, temp, int(actual.stat.Uid), int(actual.stat.Gid), unix.AT_SYMLINK_NOFOLLOW)
			if err == nil {
				err = copyAttributes(parent, name, temp)
			}
		}

		return fault.Wrap("create incoming symlink", err)
	}

	fd, err := unix.Openat(parent, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fault.Wrap("create incoming temporary", err)
	}

	file := os.NewFile(uintptr(fd), temp)
	defer resource.Close(file)

	_, err = file.Write(data)
	if err != nil {
		return fault.Wrap("write incoming temporary", err)
	}

	err = preserveMetadata(parent, name, temp, fd, mode, actual)
	if err != nil {
		return err
	}

	return fault.Wrap("sync incoming temporary", file.Sync())
}

func preserveMetadata(parent int, name, temp string, fd int, mode string, actual observed) error {
	permissions := uint32(0o600)

	if actual.mode != absent {
		err := unix.Fchown(fd, int(actual.stat.Uid), int(actual.stat.Gid))
		if err != nil {
			return fault.Wrap("preserve incoming owner", err)
		}

		err = copyAttributes(parent, name, temp)
		if err != nil {
			return err
		}
	}

	if actual.mode == regular || actual.mode == executable {
		const ordinaryPermissions = 0o777

		permissions = actual.stat.Mode & ordinaryPermissions
	}

	if mode == regular {
		permissions &^= 0o111
	} else if permissions&0o111 == 0 {
		permissions |= 0o100
	}

	return fault.Wrap("set incoming permissions", unix.Fchmod(fd, permissions))
}

func copyAttributes(parent int, name, temp string) error {
	// procfs pins the parent; L* operations never follow the final source or target link.
	source := fmt.Sprintf("/proc/self/fd/%d/%s", parent, name)
	target := fmt.Sprintf("/proc/self/fd/%d/%s", parent, temp)

	const maxAttributes = 1 << 20

	buffer := make([]byte, maxAttributes)

	count, err := unix.Llistxattr(source, buffer)
	if errors.Is(err, unix.ENOTSUP) {
		return nil
	}

	if err != nil {
		return fault.Wrap("list incoming metadata", err)
	}

	for attribute := range strings.SplitSeq(string(buffer[:count]), "\x00") {
		if attribute == "" {
			continue
		}

		count, err = unix.Lgetxattr(source, attribute, buffer)
		if err == nil {
			err = unix.Lsetxattr(target, attribute, buffer[:count], 0)
		}

		if err != nil {
			return fault.Wrap("preserve incoming metadata", err)
		}
	}

	return nil
}
