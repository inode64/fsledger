package integrity

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/resource"
)

// NFS rejects renameat2 flags with EINVAL: the fallback must still create a missing name, keep an existing
// one and leave no temporary name behind.
func TestRenameNoReplaceFallsBackWhereFlagsAreRejected(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()

	parent, err := unix.Open(directory, DirectoryFlags, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.FD(parent)

	rejecting := func(int, string, int, string, uint) error { return unix.EINVAL }

	for _, name := range []string{"temporary", "existing"} {
		err = os.WriteFile(filepath.Join(directory, name), []byte(name), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	err = renameNoReplace(parent, "temporary", "existing", rejecting)
	if !errors.Is(err, unix.EEXIST) {
		t.Fatal("an existing name was not kept:", err)
	}

	err = renameNoReplace(parent, "temporary", "restored", rejecting)
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(directory, "restored")) //nolint:gosec // Inside the test's directory.
	if err != nil || string(data) != "temporary" {
		t.Fatalf("restored %q: %v", data, err)
	}

	_, err = os.Lstat(filepath.Join(directory, "temporary"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the temporary name was left behind:", err)
	}
}
