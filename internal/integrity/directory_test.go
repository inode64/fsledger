package integrity_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/exclude"
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

// A chain reaching PATH_MAX is left unobserved below that point and reported as unstable: walking it would
// hold a descriptor per level, and its recorded entries must not look deleted.
func TestScanLeavesPathsPastPathMaxUnstable(t *testing.T) {
	t.Parallel()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	name := strings.Repeat("d", 250)

	directory, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	for level := 0; err == nil && level < 20; level++ {
		err = unix.Mkdirat(directory, name, 0o700)

		var next int

		if err == nil {
			next, err = unix.Openat(directory, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		}

		resource.FD(directory)
		directory = next
	}

	if err != nil {
		t.Fatal(err)
	}

	resource.FD(directory)

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	visited := 0

	err = integrity.NewScanner(1, 1).ScanPaths(t.Context(), []string{root}, nil, matcher, "sha256", false, true,
		func(integrity.Record) error {
			visited++

			return nil
		}, nil)

	paths, failure := integrity.SplitUnstable(err)
	if failure != nil || len(paths) != 1 || len(paths[0]) < unix.PathMax || visited > unix.PathMax/len(name)+1 {
		t.Fatal("deep chain walked or not reported", visited, paths, failure)
	}
}
