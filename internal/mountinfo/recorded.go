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
	return !volatile(mount.Type) && !userMounted(mount) && !shows(mount, table)
}

// Mounted reports whether a filesystem is visible at point: the mount no other one at the point sits on,
// unless it shows the directory it covers, as a directory bound onto itself does even with a volume tucked
// beneath it, or is an automounter's own entry, left when reaching the point mounted nothing.
func Mounted(point string, table []Mount) bool {
	for _, mount := range table {
		if mount.Point == point && !slices.ContainsFunc(table, func(other Mount) bool {
			return other.Point == point && other.Parent == mount.ID
		}) {
			return mount.Type != "autofs" && !shows(mount, table)
		}
	}

	return false
}

// OnRemote reports a path on a remote filesystem, where nothing may be probed: an unreachable server would
// block the caller.
func OnRemote(name string, table []Mount) bool {
	holding := Resolve(name, table)

	return len(holding) > 0 && holding[0].Remote()
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

// userMounted reports a filesystem that an unprivileged user mounted and may unmount: a FUSE mount of a user
// other than root, or an eCryptfs private directory, which a setuid helper mounts at login.
func userMounted(mount Mount) bool {
	return strings.HasPrefix(mount.Type, "fuse") && mount.UserID != "" && mount.UserID != "0" ||
		mount.Type == "ecryptfs"
}
