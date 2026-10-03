package integrity

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/inode64/fsledger/internal/pathutil"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

// HashSource supplies bytes hashed during this same serialized mirror operation.
// Implementations must match the source identity and metadata, never a prior scan.
type HashSource func(Record, string) (string, int64, bool)

// readWorkspace is owned exclusively by one scanner slot while observing a file.
// Buffers are allocated lazily: metadata-only scans retain no content buffers.
type readWorkspace struct {
	digest    hash.Hash
	algorithm string
	buffer    []byte
}

func (workspace *readWorkspace) hasher(algorithm string) (hash.Hash, error) {
	if workspace.digest == nil || workspace.algorithm != algorithm {
		digest, err := Hasher(algorithm)
		if err != nil {
			return nil, err
		}

		workspace.digest, workspace.algorithm = digest, algorithm
	}

	if workspace.buffer == nil {
		workspace.buffer = make([]byte, bufferSize)
	}

	workspace.digest.Reset()

	return workspace.digest, nil
}

// ErrUnstable rejects metadata or hashes observed across a concurrent source mutation.
var ErrUnstable = errors.New("source changed during observation")

const (
	bufferSize         = 64 << 10
	maxAttributesBytes = 1 << 20
)

// Missing reports a source name that no longer exists without following symlinks.
// OpenParent yields ENOTDIR when a parent became a file or a symlink; every name
// below it is gone, and the replacement is captured by the parent's own event.
func Missing(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOTDIR)
}

// OpenParent anchors every source component without following symlinks.
func OpenParent(path string) (int, string, error) {
	if !pathutil.ValidAbsolute(path) {
		return -1, "", fault.New("source path must be clean and absolute")
	}

	fd, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", fault.Wrap("open source root", err)
	}

	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for _, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(fd, part, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		resource.FD(fd)

		if openErr != nil {
			return -1, "", fault.Wrap("open source parent", openErr)
		}

		fd = next
	}

	name := parts[len(parts)-1]
	if name == "" {
		name = "."
	}

	return fd, name, nil
}

func (workspace *readWorkspace) read(
	ctx context.Context,
	path, algorithm string,
	content bool,
	copied HashSource,
) (Record, error) {
	err := ctx.Err()
	if err != nil {
		return Record{}, fault.Wrap("observation cancelled", err)
	}

	parent, name, err := OpenParent(path)
	if err != nil {
		return Record{}, err
	}
	defer resource.FD(parent)

	var stat unix.Stat_t

	err = unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return Record{}, fault.Wrap("stat source", err)
	}

	record := fromStat(path, &stat)

	readBirthTime(parent, name, &record)

	if record.Type == TypeSymlink {
		record.Target, err = readLink(parent, name)
		if err != nil {
			return Record{}, err
		}
	}

	if record.Type == TypeRegular && content && !reuseCopiedHash(copied, algorithm, &record) {
		err = workspace.hashFile(ctx, parent, name, algorithm, &stat, &record)
		if err != nil {
			return Record{}, err
		}
	}

	// /proc/self/fd anchors the parent; L* operations never dereference the final symlink.
	anchored := fmt.Sprintf("/proc/self/fd/%d/%s", parent, name)

	err = readAttributes(anchored, &record)
	if err != nil {
		return Record{}, err
	}

	var after unix.Stat_t

	err = unix.Fstatat(parent, name, &after, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return Record{}, fault.Wrap("restat source", err)
	}

	if !sameVersion(&stat, &after) {
		return Record{}, fmt.Errorf("%w: %s", ErrUnstable, path)
	}

	return record, nil
}

func fromStat(path string, stat *unix.Stat_t) Record {
	record := Record{
		Path: []byte(path), Type: FileType(stat.Mode), Mode: stat.Mode, UID: stat.Uid, GID: stat.Gid,
		Size: stat.Size, Device: stat.Dev, Inode: stat.Ino, Nlink: stat.Nlink,
		Atime: stat.Atim.Nano(), Mtime: stat.Mtim.Nano(), Ctime: stat.Ctim.Nano(), Observed: time.Now().UnixNano(),
	}
	if record.Type == "" {
		record.Type = TypeSpecial
	}

	return record
}

func sameVersion(first, second *unix.Stat_t) bool {
	return first.Dev == second.Dev && first.Ino == second.Ino && first.Size == second.Size &&
		first.Mode == second.Mode &&
		first.Uid == second.Uid &&
		first.Gid == second.Gid &&
		first.Mtim == second.Mtim &&
		first.Ctim == second.Ctim
}

type readerFunc func([]byte) (int, error)

func (reader readerFunc) Read(buffer []byte) (int, error) { return reader(buffer) }

func (workspace *readWorkspace) hashFile(
	ctx context.Context,
	parent int,
	name, algorithm string,
	before *unix.Stat_t,
	record *Record,
) error {
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	fd, err := unix.Openat(parent, name, flags|unix.O_NOATIME, 0)

	record.NoAtime = err == nil
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EINVAL) {
		fd, err = unix.Openat(parent, name, flags, 0)
	}

	if err != nil {
		return fault.Wrap("open source for hashing", err)
	}

	file := os.NewFile(uintptr(fd), name)
	defer resource.Close(file)

	var opened unix.Stat_t

	err = unix.Fstat(fd, &opened)
	if err != nil {
		return fault.Wrap("stat opened source", err)
	}

	if opened.Mode&unix.S_IFMT != unix.S_IFREG || !sameVersion(before, &opened) {
		return fmt.Errorf("%w: source replaced before hashing", ErrUnstable)
	}

	digest, err := workspace.hasher(algorithm)
	if err != nil {
		return err
	}

	count, err := io.CopyBuffer(digest, io.LimitReader(readerFunc(func(buffer []byte) (int, error) {
		ctxErr := ctx.Err()
		if ctxErr != nil {
			return 0, fault.Wrap("hash cancelled", ctxErr)
		}

		return file.Read(buffer)
	}), before.Size), workspace.buffer)
	if err != nil {
		return fault.Wrap("hash source", err)
	}

	err = verifyHashedFile(file, before, count)
	if err != nil {
		return err
	}

	record.HashedAt = time.Now().UnixNano()
	record.Hash, record.Algorithm = hex.EncodeToString(digest.Sum(nil)), algorithm

	return nil
}

func verifyHashedFile(file *os.File, before *unix.Stat_t, count int64) error {
	sizeErr := CheckContentSize(file, before.Size, count)

	var after unix.Stat_t

	err := unix.Fstat(int(file.Fd()), &after)
	if err != nil {
		return fault.Wrap("stat hashed source", err)
	}

	if !sameVersion(before, &after) {
		return fmt.Errorf("%w: source changed during hashing", ErrUnstable)
	}

	return sizeErr
}

func readAttributes(path string, record *Record) error {
	size, err := unix.Llistxattr(path, nil)
	if errors.Is(err, unix.ENOTSUP) {
		record.AttributesStatus = "unsupported"

		return nil
	}

	if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) {
		return fault.New("extended attributes are inaccessible")
	}

	if err != nil {
		return fault.Wrap("list extended attributes", err)
	}

	if size > maxAttributesBytes {
		return fault.New("extended attributes exceed observation limit")
	}

	names := make([]byte, max(size, 1))

	size, err = unix.Llistxattr(path, names)

	err = attributeReadError(size, len(names), err)
	if err != nil {
		return fault.Wrap("read extended attribute names", err)
	}

	record.AttributesStatus = "available"

	remaining := maxAttributesBytes - size
	for name := range bytes.SplitSeq(names[:size], []byte{0}) {
		if len(name) == 0 {
			continue
		}

		attribute, getErr := readAttribute(path, name, remaining)
		if getErr != nil {
			return getErr
		}

		remaining -= len(attribute.Value)

		if string(name) == "system.posix_acl_access" || string(name) == "system.posix_acl_default" {
			record.ACL = append(record.ACL, attribute)
		} else {
			record.Xattrs = append(record.Xattrs, attribute)
		}
	}

	compare := func(first, second Attribute) int { return bytes.Compare(first.Name, second.Name) }
	slices.SortFunc(record.Xattrs, compare)
	slices.SortFunc(record.ACL, compare)

	return nil
}

func readAttribute(path string, name []byte, remaining int) (Attribute, error) {
	count, err := unix.Lgetxattr(path, string(name), nil)

	err = attributeReadError(count, maxAttributesBytes, err)
	if err != nil {
		return Attribute{}, fault.Wrap("inspect extended attribute", err)
	}

	if count > remaining {
		return Attribute{}, fault.New("extended attributes exceed observation limit")
	}

	value := make([]byte, max(count, 1))

	count, err = unix.Lgetxattr(path, string(name), value)

	err = attributeReadError(count, min(len(value), remaining), err)
	if err != nil {
		return Attribute{}, fault.Wrap("read extended attribute", err)
	}

	return Attribute{Name: bytes.Clone(name), Value: value[:count]}, nil
}

func readBirthTime(parent int, name string, record *Record) {
	var extended unix.Statx_t
	if unix.Statx(parent, name, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &extended) == nil &&
		extended.Mask&unix.STATX_BTIME != 0 {
		record.HasBtime = true
		record.Btime = extended.Btime.Sec*int64(time.Second) + int64(extended.Btime.Nsec)
	}
}

// A size query and the subsequent read are not atomic. Reject raced buffers and
// disappearing attributes so Scanner can retry without accepting partial metadata.
func attributeReadError(count, capacity int, err error) error {
	if errors.Is(err, unix.ERANGE) || errors.Is(err, unix.ENODATA) || (err == nil && (count < 0 || count > capacity)) {
		return ErrUnstable
	}

	return err
}

func reuseCopiedHash(source HashSource, algorithm string, record *Record) bool {
	if source == nil {
		return false
	}

	digest, when, ok := source(*record, algorithm)
	if ok {
		record.Hash, record.HashedAt, record.Algorithm = digest, when, algorithm
	}

	return ok
}

func readLink(parent int, name string) ([]byte, error) {
	target := make([]byte, bufferSize)

	count, err := unix.Readlinkat(parent, name, target)
	if err != nil {
		return nil, fault.Wrap("read source symlink", err)
	}

	return target[:count], nil
}
