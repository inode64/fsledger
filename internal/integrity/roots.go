package integrity

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

// ErrUnavailable means a configured source cannot currently be interpreted as an empty tree.
var ErrUnavailable = errors.New("configured source unavailable")

// RootGuard pins each source filesystem until that source is forgotten. A file root
// is anchored at its parent so deleting that file remains a normal observation.
type RootGuard struct {
	roots map[string]rootIdentity
	mutex sync.Mutex
}

type rootIdentity struct {
	device    uint64
	mount     uint64
	directory bool
}

// Check rejects missing directory roots and filesystem replacements before pruning.
func (guard *RootGuard) Check(roots []string) error {
	guard.mutex.Lock()
	defer guard.mutex.Unlock()

	if guard.roots == nil {
		guard.roots = make(map[string]rootIdentity)
	}

	for _, root := range roots {
		expected, known := guard.roots[root]

		current, err := inspectRoot(root, expected, known)
		if err != nil {
			return fmt.Errorf("%w: %s: %w", ErrUnavailable, root, err)
		}

		if expected.mount != 0 && current.mount == 0 {
			return fmt.Errorf("%w: mount identity unavailable at %s", ErrUnavailable, root)
		}

		if known && (expected.device != current.device ||
			(expected.mount != 0 && current.mount != 0 && expected.mount != current.mount)) {
			return fmt.Errorf("%w: filesystem changed at %s", ErrUnavailable, root)
		}

		guard.roots[root] = current
	}

	return nil
}

func inspectRoot(path string, expected rootIdentity, known bool) (rootIdentity, error) {
	if known && !expected.directory {
		return inspectFileRoot(path)
	}

	current, err := identify(path)
	if err != nil {
		return rootIdentity{}, err
	}

	if !known && !current.directory {
		return inspectFileRoot(path)
	}

	return current, nil
}

func inspectFileRoot(path string) (rootIdentity, error) {
	actual, err := identify(path)
	if err == nil && actual.directory {
		return actual, nil
	}

	if err != nil && !Missing(err) {
		return rootIdentity{}, err
	}

	current, err := identify(filepath.Dir(path))
	if err != nil {
		return rootIdentity{}, err
	}

	if !current.directory {
		return rootIdentity{}, fault.New("source parent is not a directory")
	}

	current.directory = false

	return current, nil
}

// identify reads the filesystem identity of one name without following symlinks.
func identify(path string) (rootIdentity, error) {
	parent, name, err := OpenParent(path)
	if err != nil {
		return rootIdentity{}, err
	}
	defer resource.FD(parent)

	var stat unix.Stat_t

	err = unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return rootIdentity{}, fault.Wrap("stat source root", err)
	}

	var extended unix.Statx_t

	mount := uint64(0)

	err = statxRetry(parent, name, unix.STATX_MNT_ID, &extended)
	if err != nil && !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EOPNOTSUPP) {
		return rootIdentity{}, fault.Wrap("stat source mount", err)
	}

	if err == nil && extended.Mask&unix.STATX_MNT_ID != 0 {
		mount = extended.Mnt_id
	}

	return rootIdentity{device: stat.Dev, mount: mount, directory: stat.Mode&unix.S_IFMT == unix.S_IFDIR}, nil
}

// Forget releases pinned identities so a removed source may later return in another form.
func (guard *RootGuard) Forget(roots []string) {
	guard.mutex.Lock()
	defer guard.mutex.Unlock()

	for _, root := range roots {
		delete(guard.roots, root)
	}
}

// Unavailable separates a lost filesystem from a deletion: a pinned root is unavailable when
// it is missing and its nearest existing ancestor is not the filesystem it was pinned on.
func (guard *RootGuard) Unavailable(root string) bool {
	guard.mutex.Lock()
	defer guard.mutex.Unlock()

	expected, known := guard.roots[root]
	if !known {
		return false
	}

	// A file root pins its parent, so that parent is already an ancestor: after an unmount it
	// still exists as the empty mountpoint and must be compared, not taken as the root present.
	path, missing := root, false
	if !expected.directory {
		path, missing = filepath.Dir(root), true
	}

	for ; ; path, missing = filepath.Dir(path), true {
		current, err := identify(path)
		if err == nil {
			return missing && (current.device != expected.device ||
				(expected.mount != 0 && current.mount != expected.mount))
		}

		if !Missing(err) || path == "/" {
			return true
		}
	}
}

func statxRetry(parent int, name string, mask int, stat *unix.Statx_t) error {
	for {
		err := unix.Statx(parent, name, unix.AT_SYMLINK_NOFOLLOW, mask, stat)
		if !errors.Is(err, unix.EINTR) {
			return fault.Wrap("statx source", err)
		}
	}
}
