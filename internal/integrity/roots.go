package integrity

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
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
	mutex sync.Mutex
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

	var (
		mounts  []mountinfo.Mount
		readErr error
		read    bool
	)

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
			// One table serves every root this call pins or rechecks.
			if !read {
				mounts, readErr = mountinfo.Read()
				read = true
			}

			current.point = mountPoint(root, current.directory, mounts, readErr)

			if changed && !remounted(expected, current, mounts, readErr) {
				return fmt.Errorf("%w: filesystem changed at %s", ErrUnavailable, root)
			}
		}

		guard.roots[root] = current
	}

	return nil
}

// CheckMounts fails while a recorded mount point has no filesystem mounted, or the mount table cannot tell:
// its tree would read as empty, so nothing may be deleted or pruned.
func CheckMounts(points []string) error {
	if len(points) == 0 {
		return nil
	}

	missing, _, err := MissingMounts(points)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}

	if len(missing) == 0 {
		return nil
	}

	return fmt.Errorf("%w: filesystem not mounted at %s", ErrUnavailable, strings.Join(missing, ", "))
}

// MissingMounts lists the recorded mount points with no filesystem mounted once each was reached, as a walk
// reaches it, which mounts an automounted filesystem: one unreachable, or left with its directory or the
// automounter's entry. Nothing on a remote filesystem is reached. It returns the mount table it judged by;
// without one it lists every point, and the error says why.
func MissingMounts(points []string) ([]string, []mountinfo.Mount, error) {
	table, err := mountinfo.Read()
	if err != nil {
		return slices.Clone(points), nil, err
	}

	var unreachable []string

	reread := false

	for _, point := range points {
		if mountinfo.OnRemote(point, table) {
			continue
		}

		if ReachMount(point) != nil {
			unreachable = append(unreachable, point)
		} else if !mountinfo.Mounted(point, table) {
			reread = true
		}
	}

	if reread {
		table, err = mountinfo.Read()
		if err != nil {
			return slices.Clone(points), nil, err
		}
	}

	return slices.DeleteFunc(slices.Clone(points), func(point string) bool {
		return !slices.Contains(unreachable, point) && mountinfo.Mounted(point, table)
	}), table, nil
}

// ReachMount opens a mount point as a walk does, without following symlinks, which mounts an automounted
// filesystem there. A point that is not a directory has nothing to mount.
func ReachMount(point string) error {
	parent, name, err := OpenParent(point)
	if err != nil {
		return err
	}
	defer resource.FD(parent)

	directory, err := unix.Openat(parent, name, DirectoryFlags, 0)
	if errors.Is(err, unix.ENOTDIR) {
		return nil
	}

	if err != nil {
		return fault.Wrap("open mount point "+point, err)
	}

	resource.FD(directory)

	return nil
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

// remounted recognizes a filesystem mounted again at the point the root was pinned on, which the kernel
// gives another ID: the daemon waits while a recorded point is unmounted, so a filesystem there now is the
// source. One that left its point mid-operation leaves the root on another, and is rejected.
func remounted(expected, current rootIdentity, mounts []mountinfo.Mount, err error) bool {
	return err == nil && expected.point != "" && current.point == expected.point &&
		mountinfo.Mounted(expected.point, mounts)
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
