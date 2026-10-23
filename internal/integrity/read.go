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

// errAttributesTooLarge marks attributes beyond maxAttributesBytes, or names or a value the kernel
// refuses to return at once (E2BIG past its 64 KiB limits).
var errAttributesTooLarge = errors.New("extended attributes exceed observation limit")

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

	fd, err = Descend(fd, parts[:len(parts)-1], 0)
	if err != nil {
		return -1, "", fault.Wrap("open source parent", err)
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

	err = readBirthTime(parent, name, &record)
	if err != nil {
		return Record{}, err
	}

	if record.Type == TypeSymlink {
		record.Target, err = workspace.readLink(parent, name)
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

	err = finishObservation(parent, name, path, &stat, &record)

	return record, err
}

func finishObservation(parent int, name, path string, before *unix.Stat_t, record *Record) error {
	// /proc/self/fd anchors the parent; L* operations never dereference the final symlink.
	anchored := fmt.Sprintf("/proc/self/fd/%d/%s", parent, name)

	err := readAttributes(anchored, record)
	if err != nil {
		return err
	}

	var after unix.Stat_t

	err = unix.Fstatat(parent, name, &after, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return fault.Wrap("restat source", err)
	}

	if !sameVersion(before, &after) {
		return fmt.Errorf("%w: %s", ErrUnstable, path)
	}

	// Hashing without O_NOATIME may have advanced access time. Persist the
	// post-read value rather than stale metadata from before our own access.
	record.Atime = after.Atim.Nano()
	// Zero marks an access time the mount does not maintain: it is not shown.
	if !accessTimesMaintained(parent, anchored, record.Type) {
		record.Atime = 0
	}

	return nil
}

// accessTimesMaintained asks the mount of the observed entry whether access times are kept current.
// Directories may root another mount and are asked by path; other entries share their parent's
// mount (file bind mounts aside) and are asked through the parent descriptor already open, which
// avoids a path lookup and never follows a final symlink.
// When the mount cannot be asked, the observed access time is kept.
func accessTimesMaintained(parent int, anchored, kind string) bool {
	var (
		stat unix.Statfs_t
		err  error
	)

	if kind == TypeDirectory {
		err = unix.Statfs(anchored, &stat)
	} else {
		err = unix.Fstatfs(parent, &stat)
	}

	if err != nil {
		return true
	}

	return atimeMaintained(stat.Flags, kind == TypeDirectory)
}

func fromStat(path string, stat *unix.Stat_t) Record {
	return Record{
		Path: []byte(path), Type: FileType(stat.Mode), Mode: stat.Mode, UID: stat.Uid, GID: stat.Gid,
		Size: stat.Size, Device: stat.Dev, Inode: stat.Ino, Nlink: stat.Nlink,
		Atime: stat.Atim.Nano(), Mtime: stat.Mtim.Nano(), Ctime: stat.Ctim.Nano(), Observed: time.Now().UnixNano(),
	}
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
	fd, err := openHashed(parent, name, record)
	if err != nil {
		return err
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

// openHashed opens a name the caller saw as a regular file without following it, and without
// advancing its access time where the kernel permits.
func openHashed(parent int, name string, record *Record) (int, error) {
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	fd, err := unix.Openat(parent, name, flags|unix.O_NOATIME, 0)

	record.NoAtime = err == nil
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EINVAL) {
		fd, err = unix.Openat(parent, name, flags, 0)
	}

	// A symlink (ELOOP under O_NOFOLLOW) or a socket (ENXIO) replaced the regular file since.
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENXIO) {
		return -1, fmt.Errorf("%w: source replaced before hashing", ErrUnstable)
	}

	return fd, fault.Wrap("open source for hashing", err)
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

// readAttributes records xattrs and ACLs. Attributes too large to observe belong to the entry, not
// to the scan: they are recorded as AttributesTooLarge without a partial list, which keeps memory
// bounded while the scan, and with it deletion detection, completes.
func readAttributes(path string, record *Record) error {
	err := collectAttributes(path, record)
	if errors.Is(err, errAttributesTooLarge) {
		record.AttributesStatus, record.Xattrs, record.ACL = AttributesTooLarge, nil, nil

		return nil
	}

	return err
}

func collectAttributes(path string, record *Record) error {
	size, err := unix.Llistxattr(path, nil)
	if errors.Is(err, unix.ENOTSUP) {
		record.AttributesStatus = AttributesUnsupported

		return nil
	}

	if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) {
		return fault.New("extended attributes are inaccessible")
	}

	if err != nil {
		return fault.Wrap("list extended attributes", err)
	}

	if size > maxAttributesBytes {
		return errAttributesTooLarge
	}

	names := make([]byte, max(size, 1))

	size, err = unix.Llistxattr(path, names)

	err = attributeReadError(size, len(names), err)
	if err != nil {
		return fault.Wrap("read extended attribute names", err)
	}

	record.AttributesStatus = AttributesAvailable

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

		if string(name) == aclAccess || string(name) == aclDefault {
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
		return Attribute{}, errAttributesTooLarge
	}

	value := make([]byte, max(count, 1))

	count, err = unix.Lgetxattr(path, string(name), value)

	err = attributeReadError(count, min(len(value), remaining), err)
	if err != nil {
		return Attribute{}, fault.Wrap("read extended attribute", err)
	}

	return Attribute{Name: bytes.Clone(name), Value: value[:count]}, nil
}

func readBirthTime(parent int, name string, record *Record) error {
	var extended unix.Statx_t

	err := statxRetry(parent, name, unix.STATX_BTIME, &extended)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
		return nil
	}

	if err != nil {
		return fault.Wrap("stat source birth time", err)
	}

	if extended.Mask&unix.STATX_BTIME != 0 {
		record.HasBtime = true
		record.Btime = extended.Btime.Sec*int64(time.Second) + int64(extended.Btime.Nsec)
	}

	return nil
}

// A size query and the subsequent read are not atomic. Reject raced buffers and
// disappearing attributes so Scanner can retry without accepting partial metadata.
// E2BIG is no race: the kernel reports it only for buffers already at its limit.
func attributeReadError(count, capacity int, err error) error {
	if errors.Is(err, unix.E2BIG) {
		return errAttributesTooLarge
	}

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

// readLink reads into the reusable buffer and keeps only the target: records stay in batches.
func (workspace *readWorkspace) readLink(parent int, name string) ([]byte, error) {
	if workspace.buffer == nil {
		workspace.buffer = make([]byte, bufferSize)
	}

	count, err := unix.Readlinkat(parent, name, workspace.buffer)
	if err != nil {
		return nil, fault.Wrap("read source symlink", err)
	}

	return bytes.Clone(workspace.buffer[:count]), nil
}
