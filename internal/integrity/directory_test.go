package integrity_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/resource"
)

func TestSyncDirectoryAcceptsPinnedParent(t *testing.T) {
	t.Parallel()

	parent, err := unix.Open(t.TempDir(), integrity.DirectoryFlags, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.FD(parent)

	// Writers pin parents through O_PATH, which fsync itself refuses.
	if !errors.Is(unix.Fsync(parent), unix.EBADF) {
		t.Fatal("fixture parent is not an O_PATH descriptor")
	}

	err = integrity.SyncDirectory(parent)
	if err != nil {
		t.Fatal(err)
	}
}

func TestDescendCreatesOnlyWithModeAndRefusesSymlinks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	err := os.Symlink(root, filepath.Join(root, "link"))
	if err != nil {
		t.Fatal(err)
	}

	open := func(mode uint32, parts ...string) error {
		fd, openErr := unix.Open(root, integrity.DirectoryFlags, 0)
		if openErr == nil {
			fd, openErr = integrity.Descend(fd, parts, mode)
		}

		if openErr == nil {
			resource.FD(fd)
		}

		return openErr
	}

	if !errors.Is(open(0, "a", "b"), os.ErrNotExist) {
		t.Fatal("missing directory created without a mode")
	}

	err = open(0o700, "a", "b")
	if err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(root, "a", "b"))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatal("directory not created with its mode", info, err)
	}

	if !errors.Is(open(0o700, "link", "c"), unix.ENOTDIR) {
		t.Fatal("symlinked component followed")
	}
}
