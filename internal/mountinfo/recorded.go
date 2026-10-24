package mountinfo

import (
	"path"
	"slices"
	"strings"

	"github.com/inode64/fsledger/internal/pathutil"
)

// Recordable reports a mount whose absence must not read as an empty tree: not a volatile filesystem, whose
// contents go with the mount, not one users mount themselves, whose unmount would pause the repository at
// their will, and not a directory bound onto itself, as systemd mounts ReadWritePaths in the service's
// namespace only.
func Recordable(mount Mount, table []Mount) bool {
	return !volatile(mount.Type) && !userMounted(mount.Type) && !shows(mount, table)
}

// Mounted reports whether a filesystem is visible at point: the mount no other one at the point sits on,
// unless it shows the directory it covers, as a directory bound onto itself does even with a volume tucked
// beneath it. Any other one counts, an automounter's own entry too, since reaching the point mounts what it
// serves.
func Mounted(point string, table []Mount) bool {
	for _, mount := range table {
		if mount.Point == point && !slices.ContainsFunc(table, func(other Mount) bool {
			return other.Point == point && other.Parent == mount.ID
		}) {
			return !shows(mount, table)
		}
	}

	return false
}

// Related reports a mount point that holds one of roots, or lies inside one and is not excluded: the points a
// repository records.
func Related(point string, roots []string, excluded func(string) bool) bool {
	for _, root := range roots {
		if pathutil.Contains(point, root) || pathutil.Contains(root, point) && !excluded(point) {
			return true
		}
	}

	return false
}

// Unmounted lists the points no filesystem is mounted at.
func Unmounted(points []string, table []Mount) []string {
	return slices.DeleteFunc(slices.Clone(points), func(point string) bool { return Mounted(point, table) })
}

// shows reports a mount whose tree is the directory it covers, found on the mount below every one stacked at
// its point.
func shows(mount Mount, table []Mount) bool {
	below := mount

	for below.Point == mount.Point {
		index := slices.IndexFunc(table, func(parent Mount) bool { return parent.ID == below.Parent })
		if index < 0 {
			return false
		}

		below = table[index]
	}

	relative := strings.TrimPrefix(strings.TrimPrefix(mount.Point, below.Point), "/")

	return below.Device == mount.Device && pathutil.Contains(below.Point, mount.Point) &&
		path.Join(below.Root, relative) == mount.Root
}

func volatile(kind string) bool {
	switch kind {
	case "tmpfs", "ramfs", "devtmpfs", "overlay", "proc", "sysfs", "cgroup", "cgroup2", "nsfs", "mqueue",
		"debugfs", "tracefs", "securityfs", "pstore", "bpf", "configfs", "fusectl", "binfmt_misc",
		"hugetlbfs", "devpts", "autofs", "efivarfs", "selinuxfs", "rpc_pipefs", "nfsd":
		return true
	}

	return false
}

// userMounted reports a filesystem type that unprivileged users mount and unmount, as FUSE and eCryptfs
// homes are.
func userMounted(kind string) bool {
	return kind == "fuse" || strings.HasPrefix(kind, "fuse.") || kind == "ecryptfs"
}
