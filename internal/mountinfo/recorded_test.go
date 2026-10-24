package mountinfo_test

import (
	"testing"

	"github.com/inode64/fsledger/internal/mountinfo"
)

const (
	sharePoint = "/srv/share"
	typeExt4   = "ext4"
	typeNFS    = "nfs4"
	diskDevice = "8:1"
	diskSource = "/dev/sda1"
)

// Only filesystems whose contents outlive the mount are recorded: not volatile ones, and not a directory
// bound onto itself, which systemd mounts for ReadWritePaths in the service's namespace only.
func TestRecordableSkipsVolatileFilesystemsAndSelfBinds(t *testing.T) {
	t.Parallel()

	root := mountinfo.Mount{
		Point:  "/",
		Type:   typeExt4,
		Device: diskDevice,
		Source: diskSource,
		Root:   "/",
		ID:     1,
		Parent: 0,
	}
	table := []mountinfo.Mount{
		root,
		{
			Point:  "/srv/self",
			Type:   typeExt4,
			Device: diskDevice,
			Source: diskSource,
			Root:   "/srv/self",
			ID:     2,
			Parent: 1,
		},
		{Point: "/srv/bound", Type: typeExt4, Device: diskDevice, Source: diskSource, Root: "/data", ID: 3, Parent: 1},
		{Point: "/srv/scratch", Type: "tmpfs", Device: "0:40", Source: "none", Root: "/", ID: 4, Parent: 1},
		{Point: sharePoint, Type: typeNFS, Device: "0:41", Source: "server:/export", Root: "/", ID: 5, Parent: 1},
	}

	var recordable []string

	for _, mount := range table[1:] {
		if mountinfo.Recordable(mount, table) {
			recordable = append(recordable, mount.Point)
		}
	}

	if len(recordable) != 2 || recordable[0] != "/srv/bound" || recordable[1] != sharePoint {
		t.Fatal("wrong recordable mounts", recordable)
	}
}

// Any filesystem at a recorded point means it is mounted, whatever its device or fsid: a renamed disk or an
// automounter's own entry must not hold the repository back.
func TestUnmountedListsThePointsNothingIsMountedAt(t *testing.T) {
	t.Parallel()

	table := []mountinfo.Mount{
		{Point: sharePoint, Type: typeNFS, Device: "0:41", Source: "server:/a", Root: "/", ID: 2, Parent: 1},
		{Point: "/srv/auto", Type: "autofs", Device: "0:42", Source: "systemd-1", Root: "/", ID: 3, Parent: 1},
	}

	unmounted := mountinfo.Unmounted([]string{sharePoint, "/srv/auto", "/srv/gone"}, table)
	if len(unmounted) != 1 || unmounted[0] != "/srv/gone" {
		t.Fatal("wrong unmounted points", unmounted)
	}
}

// Only the visible mount at a point counts, and not when it shows the directory it covers: systemd binds a
// ReadWritePaths directory onto itself in the service's namespace, and a host mount propagated later is
// tucked beneath that bind, so the daemon still sees the empty directory.
func TestMountedJudgesTheVisibleMountAtThePoint(t *testing.T) {
	t.Parallel()

	const point = "/srv/data"

	root := mountinfo.Mount{
		Point:  "/",
		Type:   typeExt4,
		Device: diskDevice,
		Source: diskSource,
		Root:   "/",
		ID:     1,
		Parent: 0,
	}
	self := mountinfo.Mount{
		Point:  point,
		Type:   typeExt4,
		Device: diskDevice,
		Source: diskSource,
		Root:   point,
		ID:     5,
		Parent: 1,
	}
	volume := mountinfo.Mount{
		Point: point, Type: "xfs", Device: "253:3", Source: "/dev/mapper/vg-data", Root: "/", ID: 9,
		Parent: 1,
	}
	tucked := self
	tucked.Parent = volume.ID
	automount := mountinfo.Mount{
		Point: point, Type: "autofs", Device: "0:50", Source: "systemd-1", Root: "/", ID: 7,
		Parent: 1,
	}
	share := mountinfo.Mount{
		Point:  point,
		Type:   typeNFS,
		Device: "0:51",
		Source: "server:/x",
		Root:   "/",
		ID:     8,
		Parent: 7,
	}

	for _, test := range []struct {
		name  string
		table []mountinfo.Mount
		want  bool
	}{
		{"nothing", []mountinfo.Mount{root}, false},
		{"a directory bound onto itself", []mountinfo.Mount{root, self}, false},
		{"a volume", []mountinfo.Mount{root, volume}, true},
		{"a volume tucked beneath a self-bind", []mountinfo.Mount{root, tucked, volume}, false},
		{"an automounter", []mountinfo.Mount{root, automount}, true},
		{"a share over its automounter", []mountinfo.Mount{root, automount, share}, true},
	} {
		if mountinfo.Mounted(point, test.table) != test.want {
			t.Error(test.name, "misjudged")
		}
	}
}
