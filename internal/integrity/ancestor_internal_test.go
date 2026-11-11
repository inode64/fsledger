package integrity

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/resource"
)

// Replacing an ancestor while its directory fd stays open must not certify the old inode at the new path.
func TestFinishObservationRejectsReplacedAncestor(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	directory := filepath.Join(root, "project")

	err := os.Mkdir(directory, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(directory, "big.bin")

	err = os.WriteFile(path, []byte("old"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	parent, name, err := OpenParent(path)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		closeErr := unix.Close(parent)
		if closeErr != nil {
			t.Errorf("close observed parent: %v", closeErr)
		}
	})

	var before unix.Stat_t

	err = unix.Fstatat(parent, name, &before, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Rename(directory, filepath.Join(root, "project.prev"))
	if err != nil {
		t.Fatal(err)
	}

	err = os.Mkdir(directory, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(path, []byte("new"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = finishObservation(parent, name, path, &before, &Record{Type: TypeRegular})
	if !errors.Is(err, ErrUnstable) {
		t.Fatal("replaced ancestor was certified", err)
	}
}

func TestPathStillNamesAcceptsTheObservedInode(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "stable")

	err := os.WriteFile(path, []byte("same"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	var before unix.Stat_t

	err = unix.Lstat(path, &before)
	if err != nil {
		t.Fatal(err)
	}

	err = pathStillNames(path, &before)
	if err != nil {
		t.Fatal(err)
	}
}

// deepFile creates a regular file below root whose path exceeds PATH_MAX. The kernel refuses such a path
// whole, so it is built through os.Root, which walks one component at a time.
func deepFile(t *testing.T, root string) string {
	t.Helper()

	tree, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(tree)

	name := strings.Repeat("d", unix.NAME_MAX)
	file := filepath.Join(filepath.Join(slices.Repeat([]string{name}, unix.PathMax/len(name)+1)...), "file")

	err = tree.MkdirAll(filepath.Dir(file), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = tree.WriteFile(file, []byte("deep"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return filepath.Join(root, file)
}

// A path of PATH_MAX bytes or more must be compared with the observed inode like any other: accepting
// it unseen would certify the old inode after an ancestor was replaced.
func TestPathStillNamesComparesInodeBeyondPathMax(t *testing.T) {
	t.Parallel()

	directory := filepath.Join(t.TempDir(), "project")

	err := os.Mkdir(directory, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	path := deepFile(t, directory)

	parent, name, err := OpenParent(path)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.FD(parent) })

	var before unix.Stat_t

	err = unix.Fstatat(parent, name, &before, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		t.Fatal(err)
	}

	err = pathStillNames(path, &before)
	if err != nil {
		t.Fatal("unchanged deep path rejected", err)
	}

	err = errors.Join(os.Rename(directory, directory+".prev"), os.Mkdir(directory, 0o700))
	if err != nil {
		t.Fatal(err)
	}

	deepFile(t, directory)

	err = pathStillNames(path, &before)
	if !errors.Is(err, ErrUnstable) {
		t.Fatal("replaced ancestor was certified", err)
	}
}
