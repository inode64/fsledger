package watcher_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/resource"
	"github.com/inode64/fsledger/internal/watcher"
)

func TestWalkDirectoriesNeverFollowsSymlinks(t *testing.T) {
	t.Parallel()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()

	for _, directory := range []string{"a/b", "excluded/c", filepath.Join(outside, "secret")} {
		if !filepath.IsAbs(directory) {
			directory = filepath.Join(root, directory)
		}

		err = os.MkdirAll(directory, 0o700)
		if err != nil {
			t.Fatal(err)
		}
	}

	err = os.Symlink(outside, filepath.Join(root, "a", "link"))
	if err != nil {
		t.Fatal(err)
	}

	var visited []string

	deep, err := watcher.WalkDirectories(root, func(path string) bool {
		return path == filepath.Join(root, "excluded")
	}, func(path string, _ int) error {
		visited = append(visited, path)

		return nil
	})
	if err != nil || deep {
		t.Fatal(deep, err)
	}

	slices.Sort(visited)

	want := []string{root, filepath.Join(root, "a"), filepath.Join(root, "a", "b")}
	if !slices.Equal(visited, want) {
		t.Fatalf("visited %q, want %q", visited, want)
	}

	_, err = watcher.WalkDirectories(filepath.Join(root, "missing"), func(string) bool { return false },
		func(string, int) error {
			t.Fatal("visited a missing root")

			return nil
		})
	if err != nil {
		t.Fatal("missing root reported as failure", err)
	}
}

// A chain too deep to name is left to reconciliation instead of holding a descriptor per level.
func TestWalkDirectoriesStopsAtPathMax(t *testing.T) {
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

	visited := 0

	deep, err := watcher.WalkDirectories(root, func(string) bool { return false }, func(string, int) error {
		visited++

		return nil
	})
	if err != nil || !deep || visited == 0 || visited > unix.PathMax/len(name)+1 {
		t.Fatal("deep chain walked", visited, deep, err)
	}
}
