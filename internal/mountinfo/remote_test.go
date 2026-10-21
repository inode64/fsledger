package mountinfo_test

import (
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/mountinfo"
)

func TestRemoteFilesystemTypes(t *testing.T) {
	t.Parallel()

	for fsType, want := range map[string]bool{
		"nfs":            true,
		"nfs4":           true,
		"cifs":           true,
		"smb3":           true,
		"ceph":           true,
		"9p":             true,
		"virtiofs":       true,
		"afs":            true,
		"gfs2":           true,
		"ocfs2":          true,
		"lustre":         true,
		"fuse":           true,
		"fuse.glusterfs": true,
		"fuse.sshfs":     true,
		"ext4":           false,
		"xfs":            false,
		"btrfs":          false,
		"tmpfs":          false,
		"overlay":        false,
	} {
		mounts, err := mountinfo.Parse(strings.NewReader("1 0 0:40 / /srv rw - " + fsType + " source rw\n"))
		if err != nil {
			t.Fatal(err)
		}

		if len(mounts) != 1 || mounts[0].Type != fsType || mounts[0].Remote() != want {
			t.Errorf("%s: mounts %+v, want remote %t", fsType, mounts, want)
		}
	}
}
