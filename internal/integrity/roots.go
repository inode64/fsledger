package integrity

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/mountinfo"
	"github.com/inode64/fsledger/internal/pathutil"
	"github.com/inode64/fsledger/internal/resource"
)

// ErrUnavailable means a configured source cannot currently be interpreted as an empty tree.
var ErrUnavailable = errors.New("configured source unavailable")

// RootGuard pins each source filesystem until that source is forgotten. A file root
// is anchored at its parent so deleting that file remains a normal observation.
type RootGuard struct {
	roots map[string]rootIdentity
	// points are the mount points recorded for the sources before this process pinned them; see Expect.
	points []string
	mutex  sync.Mutex
}

type rootIdentity struct {
	// point is the mount point holding the root (for a file root, its parent) when it was pinned.
	point     string
	device    uint64
	mount     uint64
	directory bool
}

// Check rejects missing directory roots and filesystem replacements before pruning.
//
//nolint:gocognit // One pass keeps each root's checks next to the identity it pins.
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

		changed := known && (expected.device != current.device ||
			(expected.mount != 0 && expected.mount != current.mount))
		if known && !changed {
			current.point = expected.point
		} else {
			mounts, readErr := mountinfo.Read()
			current.point = mountPoint(root, current.directory, mounts, readErr)

			if changed && !remounted(expected, current, mounts, readErr) {
				return fmt.Errorf("%w: filesystem changed at %s", ErrUnavailable, root)
			}

			// After a restart nothing is pinned: a recorded mount point holding the root that is no longer
			// mounted lost its filesystem, and the directory found there is the empty mount point.
			if lost := lostMount(guard.points, root, mounts, readErr); !known && lost != "" {
				return fmt.Errorf("%w: filesystem not mounted at %s", ErrUnavailable, lost)
			}
		}

		guard.roots[root] = current
	}

	return nil
}

// Expect adds mount points recorded for the sources before this process pinned them, such as those a
// catalog kept across a restart: a root they hold is unavailable while they are not mounted.
func (guard *RootGuard) Expect(points []string) {
	guard.mutex.Lock()
	defer guard.mutex.Unlock()

	for _, point := range points {
		if !slices.Contains(guard.points, point) {
			guard.points = append(guard.points, point)
		}
	}
}

// lostMount returns the deepest of points holding path that the mount table no longer lists; without a
// table, the deepest holding it.
func lostMount(points []string, path string, mounts []mountinfo.Mount, err error) string {
	lost := ""

	for _, point := range points {
		mounted := err == nil && slices.ContainsFunc(mounts, func(mount mountinfo.Mount) bool {
			return mount.Point == point
		})
		if !mounted && pathutil.Contains(point, path) && len(point) > len(lost) {
			lost = point
		}
	}

	return lost
}

// RetainedMounts lists the recorded mount points within roots that are no longer mounted. Their trees were
// lost with their filesystem, not deleted, so nothing below them may be pruned. Without a mount table every
// recorded point within roots is retained.
func RetainedMounts(mounts, roots []string) []string {
	var within []string

	for _, point := range mounts {
		if pathutil.Within(roots, point) {
			within = append(within, point)
		}
	}

	if len(within) == 0 {
		return nil
	}

	table, err := mountinfo.Read()
	if err != nil {
		return within
	}

	return slices.DeleteFunc(within, func(point string) bool {
		return slices.ContainsFunc(table, func(mount mountinfo.Mount) bool { return mount.Point == point })
	})
}

// mountPoint is the deepest mount point holding a root, or its parent for a file root; "" without a mount
// table, which leaves a remount unrecognized.
func mountPoint(root string, directory bool, mounts []mountinfo.Mount, err error) string {
	if !directory {
		root = filepath.Dir(root)
	}

	resolved := mountinfo.Resolve(root, mounts)
	if err != nil || len(resolved) == 0 || !pathutil.Contains(resolved[0].Point, root) {
		return ""
	}

	return resolved[0].Point
}

// remounted recognizes a filesystem mounted again at the point the root was pinned on, which is the same
// source although the kernel may give it another ID. The pinned mount must have left that point: one stacked
// over it hides the recorded tree, and its empty contents must not be recorded. IDs are reused, so only the
// pinned ID at that point, under another mount, means stacking.
func remounted(expected, current rootIdentity, mounts []mountinfo.Mount, err error) bool {
	if err != nil || expected.mount == 0 || expected.point == "" || current.point != expected.point {
		return false
	}

	return current.mount == expected.mount || !slices.ContainsFunc(mounts, func(mount mountinfo.Mount) bool {
		return mount.Point == expected.point && mount.ID >= 0 && uint64(mount.ID) == expected.mount
	})
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

	return rootIdentity{
		point: "", device: stat.Dev, mount: mount, directory: stat.Mode&unix.S_IFMT == unix.S_IFDIR,
	}, nil
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
// it is missing and its nearest existing ancestor is not the filesystem it was pinned on. One not
// pinned yet, as after a restart, is unavailable while a recorded mount point holding it is not mounted.
func (guard *RootGuard) Unavailable(root string) bool {
	guard.mutex.Lock()
	defer guard.mutex.Unlock()

	expected, known := guard.roots[root]
	if !known {
		mounts, err := mountinfo.Read()

		return lostMount(guard.points, root, mounts, err) != ""
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
