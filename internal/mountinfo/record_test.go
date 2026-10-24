package mountinfo_test

import (
	"testing"

	"github.com/inode64/fsledger/internal/mountinfo"
)

// A remote filesystem is recognized by its type, root and source, without statfs; another filesystem at
// the point, or one stacked over it, is not the recorded one.
func TestRecordIsMountedOnlyWhileTheSameFilesystemIsOnTop(t *testing.T) {
	t.Parallel()

	share := mountinfo.Mount{
		Point: "/mnt/share", Type: "nfs4", Device: "0:50", Source: "server:/export",
		Root: "/", ID: 30, Parent: 1,
	}
	record := mountinfo.Describe(share)

	other := share
	other.Source, other.ID = "other:/export", 31

	stacked := mountinfo.Mount{
		Point: "/mnt/share", Type: "ext4", Device: "8:17", Source: "/dev/sdb1",
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
