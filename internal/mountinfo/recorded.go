package mountinfo

import (
	"path"
	"slices"
	"strings"

	"github.com/inode64/fsledger/internal/pathutil"
)

// Recordable reports a mount whose absence must not read as an empty tree: not a volatile filesystem, whose
// contents go with the mount, and not a directory bound onto itself, as systemd mounts ReadWritePaths in the
// service's namespace only.
func Recordable(mount Mount, table []Mount) bool {
	return !volatile(mount.Type) && !boundOntoItself(mount, table)
}

// Mounted reports whether some filesystem is mounted at point. Any one counts, an automounter's own entry
// too, since reaching the point mounts what it serves.
func Mounted(point string, table []Mount) bool {
	return slices.ContainsFunc(table, func(mount Mount) bool { return mount.Point == point })
}

// Unmounted lists the points no filesystem is mounted at.
func Unmounted(points []string, table []Mount) []string {
	return slices.DeleteFunc(slices.Clone(points), func(point string) bool { return Mounted(point, table) })
}

func boundOntoItself(mount Mount, table []Mount) bool {
	for _, parent := range table {
		if parent.ID != mount.Parent || parent.Point == mount.Point {
			continue
		}

		relative := strings.TrimPrefix(strings.TrimPrefix(mount.Point, parent.Point), "/")

		return parent.Device == mount.Device && pathutil.Contains(parent.Point, mount.Point) &&
			path.Join(parent.Root, relative) == mount.Root
	}

	return false
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
