package integrity

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/inode64/fsledger/internal/exclude"
)

func scannedPaths(t *testing.T, root string, paths []string, directories bool) []string {
	t.Helper()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	var (
		mutex   sync.Mutex
		visited []string
	)

	err = NewScanner(2, 2).ScanPaths(t.Context(), []string{root}, paths, matcher, "sha256", false, directories,
		func(record Record) error {
			mutex.Lock()
			defer mutex.Unlock()

			visited = append(visited, string(record.Path))

			return nil
		})
	if err != nil {
		t.Fatal(err)
	}

	slices.Sort(visited)

	return visited
}

// Without directories every file and link below the root is still observed, but no directory is.
func TestScanWithoutDirectoriesObservesOnlyEntries(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	nested := filepath.Join(root, "uploads", "2026")
	file := filepath.Join(nested, "shell.php")
	top := filepath.Join(root, "index.php")
	link := filepath.Join(root, "latest")

	err := os.MkdirAll(nested, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{file, top} {
		err = os.WriteFile(path, []byte("<?php"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	err = os.Symlink(nested, link)
	if err != nil {
		t.Fatal(err)
	}

	without := scannedPaths(t, root, nil, false)
	if want := []string{top, link, file}; !slices.Equal(without, sortedCopy(want)) {
		t.Fatalf("without directories visited %q, want %q", without, sortedCopy(want))
	}

	if scoped := scannedPaths(t, root, []string{nested}, false); !slices.Equal(scoped, []string{file}) {
		t.Fatalf("scoped scan without directories visited %q", scoped)
	}

	if single := scannedPaths(t, root, []string{top}, false); !slices.Equal(single, []string{top}) {
		t.Fatalf("file scope without directories visited %q", single)
	}

	with := scannedPaths(t, root, nil, true)

	want := sortedCopy([]string{root, filepath.Join(root, "uploads"), nested, file, top, link})
	if !slices.Equal(with, want) {
		t.Fatalf("with directories visited %q, want %q", with, want)
	}
}

func sortedCopy(paths []string) []string {
	sorted := slices.Clone(paths)
	slices.Sort(sorted)

	return sorted
}
