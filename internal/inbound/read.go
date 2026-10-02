package inbound

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/resource"
)

const maxFileBytes = 64 << 20

type observed struct {
	mode string
	data []byte
	stat unix.Stat_t
}

func readAt(parent int, name string) (observed, error) {
	var stat unix.Stat_t

	err := unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, os.ErrNotExist) {
		return observed{mode: absent, data: nil, stat: unix.Stat_t{}}, nil
	}

	if err != nil {
		return observed{}, fault.Wrap("stat incoming destination", err)
	}

	switch stat.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		if stat.Nlink != 1 || stat.Mode&0o7000 != 0 {
			return observed{}, conflict(
				"incoming replacement does not support hardlinks or special permission bits: " + name,
			)
		}

		return readRegular(parent, name, stat)
	case unix.S_IFLNK:
		const symlinkBytes = 4096

		data := make([]byte, symlinkBytes)

		length, readErr := unix.Readlinkat(parent, name, data)
		if readErr != nil {
			return observed{}, fault.Wrap("read incoming symlink", readErr)
		}

		return observed{data: data[:length], stat: stat, mode: symlink}, nil
	default:
		return observed{}, conflict("destination is a directory or special file: " + name)
	}
}

func readRegular(parent int, name string, before unix.Stat_t) (observed, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return observed{}, fault.Wrap("open incoming destination", err)
	}

	file := os.NewFile(uintptr(fd), name)
	defer resource.Close(file)

	var actual unix.Stat_t

	err = unix.Fstat(fd, &actual)
	if err != nil {
		return observed{}, fault.Wrap("stat incoming file", err)
	}

	if !sameStat(before, actual) || actual.Size > maxFileBytes {
		return observed{}, conflict("destination changed or exceeds 64 MiB: " + name)
	}

	data, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	if err != nil {
		return observed{}, fault.Wrap("read incoming destination", err)
	}

	err = unix.Fstat(fd, &actual)
	if err != nil || !sameStat(before, actual) {
		return observed{}, conflict("destination changed during read: " + name)
	}

	mode := regular
	if actual.Mode&0o111 != 0 {
		mode = executable
	}

	return observed{data: data, stat: actual, mode: mode}, nil
}

func sameStat(first, second unix.Stat_t) bool {
	return first.Dev == second.Dev && first.Ino == second.Ino && first.Mode == second.Mode &&
		first.Size == second.Size && first.Mtim == second.Mtim && first.Ctim == second.Ctim
}

func matches(ctx context.Context, objects Objects, actual observed, version Version) (bool, error) {
	if actual.mode != version.Mode {
		return false, nil
	}

	if version.Mode == absent {
		return true, nil
	}

	data, err := objects.ReadBlob(ctx, version.Object)

	return err == nil && bytes.Equal(data, actual.data), err
}

func preflight(ctx context.Context, objects Objects, change Change) error {
	_, err := targetBytes(ctx, objects, change.After)
	if err != nil {
		return err
	}

	parent, name, err := integrity.OpenParent(string(change.Path))
	if errors.Is(err, os.ErrNotExist) && change.Before.Mode == absent {
		return nil
	}

	if err != nil {
		return err
	}

	defer resource.FD(parent)

	actual, err := readAt(parent, name)
	if err != nil {
		return err
	}

	equal, err := matches(ctx, objects, actual, change.Before)
	if err != nil {
		return err
	}

	if !equal {
		return conflict("source differs from incoming base: " + string(change.Path))
	}

	return nil
}

func targetBytes(ctx context.Context, objects Objects, version Version) ([]byte, error) {
	if version.Mode == absent {
		return nil, nil
	}

	data, err := objects.ReadBlob(ctx, version.Object)
	if err != nil {
		return nil, err
	}

	if version.Mode == symlink && (len(data) == 0 || bytes.ContainsRune(data, 0) || len(data) > 4095) {
		return nil, conflict("invalid incoming symlink target")
	}

	return data, nil
}

// Parent path errors from symlinks are conflicts, not proof of an absent destination.
func pathError(err error) error {
	if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
		return conflict("incoming destination parent is no longer a directory")
	}

	return err
}

func tempName(plan *Plan, index int) string {
	return ".fsledger-inbound-" + plan.ID + "-" + strconv.Itoa(index)
}
