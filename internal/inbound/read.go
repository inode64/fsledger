package inbound

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // Git names objects of SHA-1 repositories with it.
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/pathutil"
	"github.com/inode64/fsledger/internal/resource"
)

const (
	maxFileBytes = 64 << 20
	capabilities = "security.capability"
)

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

	err = rejectCapabilities(fd, name)
	if err != nil {
		return observed{}, err
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

// Like setuid bits, file capabilities grant privileges: remote content must neither inherit nor silently drop them.
func rejectCapabilities(fd int, name string) error {
	_, err := unix.Fgetxattr(fd, capabilities, nil)
	if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) {
		return nil
	}

	if err != nil {
		return fault.Wrap("inspect incoming capabilities", err)
	}

	return conflict("incoming replacement does not support file capabilities: " + name)
}

func sameStat(first, second unix.Stat_t) bool {
	return first.Dev == second.Dev && first.Ino == second.Ino && first.Mode == second.Mode &&
		first.Size == second.Size && first.Mtim == second.Mtim && first.Ctim == second.Ctim
}

// matches compares the bytes on disk with a recorded version by hashing them as Git names a blob, which
// spares reading the blob through two Git processes.
func matches(actual observed, version Version) bool {
	if actual.mode != version.Mode {
		return false
	}

	return version.Mode == absent || blobID(version.Object, actual.data) == version.Object
}

// blobID names data as a Git blob in the object format of identifier: SHA-256 or SHA-1. A SHA-1 match would
// need a second preimage of the recorded version, not just a collision. The header and the data are hashed
// in turn, without copying the data. Hashes never fail to write; an empty name would match nothing.
func blobID(identifier string, data []byte) string {
	digest := sha1.New() //nolint:gosec // Git's SHA-1 object name, compared against a recorded version.
	if len(identifier) == sha256.Size*2 {
		digest = sha256.New()
	}

	for _, part := range [][]byte{[]byte("blob " + strconv.Itoa(len(data)) + "\x00"), data} {
		_, err := digest.Write(part)
		if err != nil {
			return ""
		}
	}

	return hex.EncodeToString(digest.Sum(nil))
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

	if !matches(actual, change.Before) {
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
	return pathutil.InboundTemporaryPrefix + plan.ID + "-" + strconv.Itoa(index)
}
