package integrity

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
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
