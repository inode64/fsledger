package mountinfo

import (
	"slices"

	"golang.org/x/sys/unix"
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

// Describe records the filesystem mounted at m.Point.
func Describe(m Mount) Record {
	record := Record{Point: m.Point, Type: m.Type, Root: m.Root, Source: m.Source, FSID: 0}
	if !m.Remote() {
		record.FSID = fsid(m.Point)
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

// topAt returns the mount visible at point: the last one listed there, stacked over any earlier one.
func topAt(point string, table []Mount) (Mount, bool) {
	for _, mount := range slices.Backward(table) {
		if mount.Point == point {
			return mount, true
		}
	}

	return Mount{}, false
}

func fsid(point string) uint64 {
	var stat unix.Statfs_t

	if unix.Statfs(point, &stat) != nil {
		return 0
	}

	//nolint:gosec // The fsid is an opaque pair of 32-bit words, compared bit for bit.
	return uint64(uint32(stat.Fsid.Val[0]))<<32 | uint64(uint32(stat.Fsid.Val[1]))
}
