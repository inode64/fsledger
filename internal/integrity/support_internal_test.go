package integrity

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/config"
)

func TestAtimeMaintainedFollowsMountOptions(t *testing.T) {
	t.Parallel()

	for _, fixture := range []struct {
		name      string
		flags     int64
		directory bool
		want      bool
	}{
		{"strictatime-file", 0, false, true},
		{"strictatime-directory", 0, true, true},
		{"noatime", unix.ST_NOATIME, false, false},
		{"relatime", unix.ST_RELATIME, false, false},
		{"nodiratime-file", unix.ST_NODIRATIME, false, true},
		{"nodiratime-directory", unix.ST_NODIRATIME, true, false},
		{"other-flags", unix.ST_NOSUID | unix.ST_NODEV, false, true},
	} {
		if got := atimeMaintained(fixture.flags, fixture.directory); got != fixture.want {
			t.Errorf("%s: atimeMaintained = %v, want %v", fixture.name, got, fixture.want)
		}
	}
}

// A path the kernel cannot inspect is not proof that its filesystem lacks attributes.
func TestAttributeSupportReportsOnlyKernelRefusals(t *testing.T) {
	t.Parallel()

	if xattrs, acl := attributeSupport(t.TempDir() + "/missing"); !xattrs || !acl {
		t.Fatal("an absent path reported unsupported attributes")
	}

	directory := t.TempDir()

	err := unix.Lsetxattr(directory, "user.fsledger", []byte("probe"), 0)
	if errors.Is(err, unix.ENOTSUP) {
		t.Skip("temporary directory filesystem has no user xattrs")
	}

	if xattrs, _ := attributeSupport(directory); !xattrs {
		t.Fatal("a filesystem storing xattrs reported none")
	}

	// A symlink root never carries ACLs; its filesystem is the one of the directory holding it.
	link := filepath.Join(directory, "link")

	err = os.Symlink("missing", link)
	if err != nil {
		t.Fatal(err)
	}

	linkXattrs, linkACL := attributeSupport(link)
	if directoryXattrs, directoryACL := attributeSupport(directory); linkXattrs != directoryXattrs ||
		linkACL != directoryACL {
		t.Fatal("a symlink root was probed as its own filesystem")
	}
}

// A selected comparison the filesystem cannot provide is named once; supported or unselected ones are not.
func TestAttributeAdvisoriesNameUnsupportedSelections(t *testing.T) {
	t.Parallel()

	if advisories := AttributeAdvisories(config.Integrity{}, "/nonexistent", "vfat"); advisories != nil {
		t.Fatal("unselected attributes were probed", advisories)
	}

	for _, fixture := range []struct {
		name                      string
		want                      []string
		selectXattrs, selectACL   bool
		supportXattrs, supportACL bool
	}{
		{"supported", nil, true, true, true, true},
		{"no xattrs", []string{"integrity.xattrs", "integrity.acl"}, true, true, false, false},
		{"no ACLs", []string{"integrity.acl"}, true, true, true, false},
		{"ACLs unselected", nil, true, false, true, false},
	} {
		var policy config.Integrity

		policy.Xattrs, policy.ACL = fixture.selectXattrs, fixture.selectACL
		advisories := attributeAdvisories(policy, "/srv/data", "vfat", fixture.supportXattrs, fixture.supportACL)

		keys := make([]string, len(advisories))
		for index, message := range advisories {
			keys[index], _, _ = strings.Cut(message, " ")

			if !strings.Contains(message, "/srv/data (vfat)") {
				t.Errorf("%s: advisory does not name the path and filesystem: %s", fixture.name, message)
			}
		}

		if !slices.Equal(keys, fixture.want) {
			t.Errorf("%s: got %q, want %q", fixture.name, advisories, fixture.want)
		}
	}
}
