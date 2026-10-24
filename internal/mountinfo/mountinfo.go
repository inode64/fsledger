// Package mountinfo adapts moby's parser into fsledger mount boundaries.
package mountinfo

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/inode64/fsledger/internal/pathutil"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"

	native "github.com/moby/sys/mountinfo"
)

// tablePath lists the mounts of the daemon's namespace, as Read parses them and Changes follows them.
const tablePath = "/proc/self/mountinfo"

// Mount describes a visible filesystem in the daemon's mount namespace.
type Mount struct {
	Point  string `json:"point"`
	Type   string `json:"type"`
	Device string `json:"device"`
	// Source is the mount's origin (a device, server:/export or a share tag), Root the directory of its
	// filesystem it shows, and Parent the ID of the mount it sits on. UserID is the user_id superblock option
	// FUSE sets to the user who mounted it, "" without one.
	Source string `json:"-"`
	Root   string `json:"-"`
	UserID string `json:"-"`
	ID     int    `json:"id"`
	Parent int    `json:"-"`
}

// Read reads /proc/self/mountinfo directly, without external commands.
func Read() ([]Mount, error) {
	file, err := os.Open(tablePath)
	if err != nil {
		return nil, fault.Wrap("read mount table", err)
	}
	defer resource.Close(file)

	return Parse(file)
}

// Parse delegates escaping and kernel grammar to the maintained parser.
func Parse(reader io.Reader) ([]Mount, error) {
	entries, err := native.GetMountsFromReader(reader, nil)
	if err != nil {
		return nil, fmt.Errorf("parse mountinfo: %w", err)
	}

	result := make([]Mount, 0, len(entries))
	for _, entry := range entries {
		result = append(
			result,
			Mount{
				Point:  entry.Mountpoint,
				Type:   entry.FSType,
				Device: fmt.Sprintf("%d:%d", entry.Major, entry.Minor),
				Source: entry.Source,
				Root:   entry.Root,
				UserID: superOption(entry.VFSOptions, "user_id"),
				ID:     entry.ID,
				Parent: entry.Parent,
			},
		)
	}

	return result, nil
}

// superOption returns the value of a superblock option, "" without it.
func superOption(options, name string) string {
	for option := range strings.SplitSeq(options, ",") {
		value, found := strings.CutPrefix(option, name+"=")
		if found {
			return value
		}
	}

	return ""
}

// Resolve returns the deepest containing mount and all nested mount points.
func Resolve(path string, mounts []Mount) []Mount {
	var (
		base   Mount
		nested []Mount
	)

	for _, mount := range mounts {
		if pathutil.Contains(mount.Point, path) && len(mount.Point) >= len(base.Point) {
			base = mount
		}

		if mount.Point != path && pathutil.Contains(path, mount.Point) {
			nested = append(nested, mount)
		}
	}

	sort.Slice(nested, func(i, j int) bool { return nested[i].Point < nested[j].Point })

	if base.Point != "" {
		return append([]Mount{base}, nested...)
	}

	return nested
}

// Nested lists the mount points below path among its boundaries. Each has a watcher of its own, so the
// walk of path leaves them out.
func Nested(path string, boundaries []Mount) []string {
	var nested []string

	for _, mount := range boundaries {
		if mount.Point != path && pathutil.Contains(path, mount.Point) {
			nested = append(nested, mount.Point)
		}
	}

	return nested
}

// Remote marks filesystems where other clients' changes may be invisible locally:
// network and cluster filesystems, host shares, and FUSE (fuse.<subtype>), whose
// daemon may serve remote storage such as GlusterFS or sshfs.
func (m Mount) Remote() bool {
	return strings.HasPrefix(m.Type, "nfs") || strings.HasPrefix(m.Type, "fuse") || m.Type == "cifs" ||
		m.Type == "smb3" ||
		m.Type == "ceph" ||
		m.Type == "cephfs" ||
		m.Type == "9p" ||
		m.Type == "virtiofs" ||
		m.Type == "afs" ||
		m.Type == "gfs2" ||
		m.Type == "ocfs2" ||
		m.Type == "lustre"
}
