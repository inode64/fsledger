package mountinfo_test

import (
	"testing"

	"github.com/inode64/fsledger/internal/mountinfo"
)

const (
	typeExt4   = "ext4"
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
		{Point: "/srv/share", Type: typeNFS, Device: "0:41", Source: "server:/export", Root: "/", ID: 5, Parent: 1},
	}

	var recordable []string

	for _, mount := range table[1:] {
		if mountinfo.Recordable(mount, table) {
			recordable = append(recordable, mount.Point)
		}
	}

	if len(recordable) != 2 || recordable[0] != "/srv/bound" || recordable[1] != "/srv/share" {
		t.Fatal("wrong recordable mounts", recordable)
	}
}
