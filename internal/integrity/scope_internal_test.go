package integrity

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/exclude"
)

func TestScopedScanGuardsConfiguredRootsOnly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	child := filepath.Join(root, "child")

	err := os.Mkdir(child, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	scanner := NewScanner(1, 2)

	visit := func(Record) error { return nil }

	err = scanner.ScanPaths(t.Context(), []string{root}, []string{child}, matcher, "sha256", false, visit)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Remove(child)
	if err != nil {
		t.Fatal(err)
	}

	err = scanner.ScanPaths(t.Context(), []string{root}, []string{child}, matcher, "sha256", false, visit)
	if err != nil {
		t.Fatal("temporary directory was pinned as a configured root", err)
	}

	err = os.Remove(root)
	if err != nil {
		t.Fatal(err)
	}

	err = scanner.ScanPaths(t.Context(), []string{root}, []string{child}, matcher, "sha256", false, visit)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing configured root accepted", err)
	}
}

func TestScopedScanRejectsEscapesAndSymlinkParents(t *testing.T) {
	t.Parallel()

	root, outside := t.TempDir(), t.TempDir()

	err := os.Symlink(outside, filepath.Join(root, "link"))
	if err != nil {
		t.Fatal(err)
	}

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	scanner := NewScanner(1, 2)

	for _, path := range []string{outside, root + "other", root + "/../escape", filepath.Join(root, "link", "child")} {
		visited := false

		err = scanner.ScanPaths(
			t.Context(),
			[]string{root},
			[]string{path},
			matcher,
			"sha256",
			false,
			func(Record) error {
				visited = true

				return nil
			},
		)
		if err == nil || visited {
			t.Fatalf("unsafe scoped path accepted: %s: %v", path, err)
		}
	}
}

func TestScopedScanPreservesSourceNamesAndTemporaryFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	names := []string{"normal", "invalid-\xff", "e\u0301.txt", ".syncthing.probe.tmp"}
	for _, name := range names {
		err := os.WriteFile(filepath.Join(root, name), []byte("fixture"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	observed := make(map[string]bool)

	err = NewScanner(1, 2).ScanPaths(t.Context(), []string{root}, []string{root}, matcher, "sha256", true,
		func(record Record) error {
			observed[filepath.Base(string(record.Path))] = true

			return nil
		})
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range names {
		//nolint:gosec // Names are fixed fixtures inside the private test directory.
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(data) != "fixture" || !observed[name] {
			t.Fatalf("source changed or omitted: %q: %v", name, err)
		}
	}
}
