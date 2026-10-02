// Package filehandle resolves Linux filesystem handles through private mount anchors.
package filehandle

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

// ErrDeleted identifies an ordinary handle lifetime race, not a coverage failure.
var ErrDeleted = errors.New("handle target was deleted")

// Handle is independent of the syscall library's representation.
type Handle struct {
	Bytes []byte
	Type  int32
	FSID  [8]byte
}

type anchor struct {
	fsid [8]byte
	fd   int
}

// Resolver owns a descriptor on the filesystem containing its root.
type Resolver struct{ anchor anchor }

// Open verifies both name-to-handle and handle-to-path access without writing sources.
func Open(path string) (*Resolver, error) {
	value, err := openAnchor(path)
	if err != nil {
		return nil, err
	}

	return &Resolver{anchor: value}, nil
}

// Path opens handles with O_PATH, so special files cannot block the reader.
func (r *Resolver) Path(handle Handle) (string, error) {
	if r.anchor.fsid != handle.FSID {
		return "", fault.New("no mount anchor for filesystem handle")
	}

	fd, err := unix.OpenByHandleAt(
		r.anchor.fd,
		unix.NewFileHandle(handle.Type, handle.Bytes),
		unix.O_PATH|unix.O_CLOEXEC,
	)
	if err != nil {
		return "", fault.Wrap("open filesystem handle", err)
	}
	defer resource.FD(fd)

	return DescriptorPath(fd)
}

// DescriptorPath names an open descriptor through procfs, or returns ErrDeleted for a detached one.
// procfs appends " (deleted)" to a detached dentry, even with surviving hardlinks, but a real name
// may end with the same text; only the inode still reachable at that name tells them apart.
func DescriptorPath(fd int) (string, error) {
	path, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return "", fault.Wrap("resolve descriptor path", err)
	}

	if strings.HasSuffix(path, " (deleted)") && !sameInode(fd, path) {
		return "", ErrDeleted
	}

	return path, nil
}

func sameInode(fd int, path string) bool {
	var opened, named unix.Stat_t

	return unix.Fstat(fd, &opened) == nil && unix.Lstat(path, &named) == nil &&
		opened.Dev == named.Dev && opened.Ino == named.Ino
}

// Close releases the mount anchor.
func (r *Resolver) Close() error {
	return fault.Wrap("close handle anchor", unix.Close(r.anchor.fd))
}

// Directory returns the directory through which a source is watched: the source itself, or the
// parent of a file, whose replacement only the parent can observe.
func Directory(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fault.Wrap("inspect source", err)
	}

	if !info.IsDir() {
		return filepath.Dir(path), nil
	}

	return path, nil
}

func openAnchor(path string) (anchor, error) {
	path, err := Directory(path)
	if err != nil {
		return anchor{}, err
	}

	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return anchor{}, fault.Wrap("open mount anchor", err)
	}

	value, err := verifyAnchor(fd, path)
	if err != nil {
		resource.FD(fd)
	}

	return value, err
}

func verifyAnchor(fd int, path string) (anchor, error) {
	var stat unix.Statfs_t

	err := unix.Fstatfs(fd, &stat)
	if err != nil {
		return anchor{}, fault.Wrap("stat mount anchor", err)
	}

	handle, _, err := unix.NameToHandleAt(unix.AT_FDCWD, path, 0)
	if err != nil {
		return anchor{}, fault.Wrap("obtain filesystem handle", err)
	}

	probe, err := unix.OpenByHandleAt(fd, handle, unix.O_PATH|unix.O_CLOEXEC)
	if err != nil {
		return anchor{}, fault.Wrap("probe filesystem handle resolution", err)
	}

	resource.FD(probe)

	value := anchor{fd: fd, fsid: [8]byte{}}
	//nolint:gosec // Preserve the signed fsid ABI bit pattern, not a numerical conversion.
	binary.NativeEndian.PutUint32(value.fsid[:4], uint32(stat.Fsid.Val[0]))
	//nolint:gosec // Preserve the signed fsid ABI bit pattern, not a numerical conversion.
	binary.NativeEndian.PutUint32(value.fsid[4:], uint32(stat.Fsid.Val[1]))

	return value, nil
}
