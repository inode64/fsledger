package mountinfo_test

import (
	"testing"

	"github.com/inode64/fsledger/internal/mountinfo"
)

const (
	sharePoint = "/srv/share"
	typeExt4   = "ext4"
	typeXFS    = "xfs"
	typeNFS    = "nfs4"
	diskDevice = "8:1"
	diskSource = "/dev/sda1"
)

// A remote filesystem is recognized by its type, root and source, without statfs; another filesystem at
// the point, or one stacked over it, is not the recorded one.
func TestRecordIsMountedOnlyWhileTheSameFilesystemIsOnTop(t *testing.T) {
	t.Parallel()

	share := mountinfo.Mount{
		Point: "/mnt/share", Type: typeNFS, Device: "0:50", Source: "server:/export",
		Root: "/", ID: 30, Parent: 1,
	}
	record := mountinfo.Describe(share)

	other := share
	other.Source, other.ID = "other:/export", 31

	stacked := mountinfo.Mount{
		Point: "/mnt/share", Type: typeExt4, Device: "8:17", Source: "/dev/sdb1",
		Root: "/", ID: 32, Parent: 30,
	}

	for _, test := range []struct {
		name  string
		table []mountinfo.Mount
		want  bool
	}{
		{"mounted", []mountinfo.Mount{share}, true},
		{"unmounted", nil, false},
		{"another filesystem", []mountinfo.Mount{other}, false},
		{"stacked over", []mountinfo.Mount{share, stacked}, false},
	} {
		if record.Mounted(test.table) != test.want {
			t.Error(test.name, "misjudged")
		}
	}
}

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

// While its recorded filesystem is not mounted, a point within a root is set aside, and so is every root it
// holds; a mounted one, or one outside every root, sets nothing aside.
func TestLostSetsAsideUnmountedPointsAndTheRootsTheyHold(t *testing.T) {
	t.Parallel()

	nested := mountinfo.Mount{
		Point: sharePoint, Type: typeNFS, Device: "0:41", Source: "server:/a",
		Root: "/", ID: 2, Parent: 1,
	}
	holding := mountinfo.Mount{
		Point: "/mnt/data", Type: typeNFS, Device: "0:42", Source: "server:/b",
		Root: "/", ID: 3, Parent: 1,
	}
	elsewhere := mountinfo.Mount{
		Point: "/elsewhere", Type: typeNFS, Device: "0:43", Source: "server:/c",
		Root: "/", ID: 4, Parent: 1,
	}

	records := []mountinfo.Record{
		mountinfo.Describe(nested), mountinfo.Describe(holding), mountinfo.Describe(elsewhere),
	}
	roots := []string{"/srv", "/mnt/data/app"}

	if kept := mountinfo.Lost(records, roots, []mountinfo.Mount{nested, holding}); len(kept) != 0 {
		t.Fatal("mounted filesystems were set aside", kept)
	}

	kept := mountinfo.Lost(records, roots, nil)
	if len(kept) != 2 || kept[0] != "/mnt/data/app" || kept[1] != sharePoint {
		t.Fatal("wrong paths set aside", kept)
	}
}

// A local filesystem is the recorded one when its fsid or its source matches: XFS takes its fsid from the
// device number, which a reboot may renumber while the source stays /dev/mapper/vg-data.
func TestLocalRecordMatchesBySourceWhenTheFsidChanges(t *testing.T) {
	t.Parallel()

	point := t.TempDir()
	record := mountinfo.Record{Point: point, Type: typeXFS, Root: "/", Source: "/dev/mapper/vg-data", FSID: 1}
	mounted := mountinfo.Mount{
		Point: point, Type: typeXFS, Device: "253:7", Source: "/dev/mapper/vg-data",
		Root: "/", ID: 40, Parent: 1,
	}

	if !record.Mounted([]mountinfo.Mount{mounted}) {
		t.Fatal("the same source was not recognized after its fsid changed")
	}

	mounted.Source = "/dev/mapper/vg-other"
	if record.Mounted([]mountinfo.Mount{mounted}) {
		t.Fatal("another device with another fsid was taken for the recorded filesystem")
	}
}
