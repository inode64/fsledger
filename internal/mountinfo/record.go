package mountinfo

import (
	"path"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/pathutil"
)

// Record identifies the filesystem mounted at a point, so it is recognized when mounted there again, after
// a restart too. A local filesystem is known by its fsid, which its UUID gives and a renamed device keeps;
// a remote one by its source, since statfs on an unreachable server blocks.
type Record struct {
	Point  string `json:"point"`
	Type   string `json:"type"`
	Root   string `json:"root"`
	Source string `json:"source,omitempty"`
	FSID   uint64 `json:"fsid,omitempty"`
}

// Describe records the filesystem mounted at mount.Point.
func Describe(mount Mount) Record {
	record := Record{Point: mount.Point, Type: mount.Type, Root: mount.Root, Source: mount.Source, FSID: 0}
	if !mount.Remote() {
		record.FSID = fsid(mount.Point)
	}

	return record
}

// Mounted reports whether the recorded filesystem is the one mounted at its point now: another filesystem
// mounted there, or stacked over it, is not.
func (r Record) Mounted(table []Mount) bool {
	top, found := topAt(r.Point, table)
	if !found || top.Type != r.Type || top.Root != r.Root {
		return false
	}

	if r.FSID != 0 {
		return fsid(r.Point) == r.FSID
	}

	return top.Source == r.Source
}

// Recordable reports a mount whose loss must not read as a deletion: not a volatile filesystem, whose
// contents go with it, and not a directory bound onto itself, as systemd mounts ReadWritePaths in the
// service's namespace only.
func Recordable(mount Mount, table []Mount) bool {
	return !volatile(mount.Type) && !boundOntoItself(mount, table)
}

// Points lists the recorded mount points.
func Points(records []Record) []string {
	points := make([]string, 0, len(records))
	for _, record := range records {
		points = append(points, record.Point)
	}

	return points
}

// topAt returns the mount visible at point: the last one listed there, stacked over any earlier one.
func topAt(point string, table []Mount) (Mount, bool) {
	for _, mount := range slices.Backward(table) {
		if mount.Point == point {
			return mount, true
		}
	}

	return Mount{}, false
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

func fsid(point string) uint64 {
	var stat unix.Statfs_t

	if unix.Statfs(point, &stat) != nil {
		return 0
	}

	//nolint:gosec // The fsid is an opaque pair of 32-bit words, compared bit for bit.
	return uint64(uint32(stat.Fsid.Val[0]))<<32 | uint64(uint32(stat.Fsid.Val[1]))
}
