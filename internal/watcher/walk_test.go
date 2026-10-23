package watcher_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

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

	err = watcher.WalkDirectories(root, func(path string) bool {
		return path == filepath.Join(root, "excluded")
	}, func(path string, _ int) error {
		visited = append(visited, path)

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	slices.Sort(visited)

	want := []string{root, filepath.Join(root, "a"), filepath.Join(root, "a", "b")}
	if !slices.Equal(visited, want) {
		t.Fatalf("visited %q, want %q", visited, want)
	}

	err = watcher.WalkDirectories(filepath.Join(root, "missing"), func(string) bool { return false },
		func(string, int) error {
			t.Fatal("visited a missing root")

			return nil
		})
	if err != nil {
		t.Fatal("missing root reported as failure", err)
	}
}
