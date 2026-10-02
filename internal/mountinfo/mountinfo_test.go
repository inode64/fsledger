package mountinfo_test

import (
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/mountinfo"
)

func TestParsingAndNestedMounts(t *testing.T) {
	t.Parallel()

	data := `1 0 8:1 / / rw - ext4 /dev/sda1 rw
2 1 0:22 / /srv/archive rw - nfs4 host:/archive rw
3 1 0:23 / /srv/with\040space rw - fuse.sshfs remote rw
`

	mounts, err := mountinfo.Parse(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	resolved := mountinfo.Resolve("/srv", mounts)
	if len(resolved) != 3 || resolved[0].Point != "/" || resolved[2].Point != "/srv/with space" ||
		!resolved[1].Remote() {
		t.Fatalf("wrong mounts: %+v", resolved)
	}

	_, parseErr := mountinfo.Parse(strings.NewReader("malformed"))
	if parseErr == nil {
		t.Fatal("invalid mountinfo accepted")
	}
}
