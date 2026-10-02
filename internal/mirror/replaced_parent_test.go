package mirror_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/mirror"
	"github.com/inode64/fsledger/internal/resource"
)

func must(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}

func replacedParentMirror(t *testing.T) (string, string, *mirror.Sync) {
	t.Helper()

	root := t.TempDir()
	for _, directory := range []string{"dir", "other"} {
		must(t, os.Mkdir(filepath.Join(root, directory), 0o700))
		must(t, os.WriteFile(filepath.Join(root, directory, "child"), []byte(directory), 0o600))
	}

	repository := t.TempDir()

	matcher, err := exclude.New()
	must(t, err)

	syncer, err := mirror.Open(repository, []string{root}, matcher, "")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.Close(syncer) })
	must(t, syncer.ReconcileContext(t.Context()))

	return root, filepath.Join(repository, strings.TrimPrefix(root, "/")), syncer
}

func TestReplacedParentDirectoryRemovesChildren(t *testing.T) {
	t.Parallel()

	replacements := map[string]func(string) error{
		"file":    func(path string) error { return os.WriteFile(path, []byte("now a file"), 0o600) },
		"symlink": func(path string) error { return os.Symlink("other", path) },
	}
	for name, replace := range replacements {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root, mirrored, syncer := replacedParentMirror(t)
			replaced := filepath.Join(root, "dir")
			child := filepath.Join(replaced, "child")

			must(t, os.RemoveAll(replaced))
			must(t, replace(replaced))

			// Both orders occur: sorted groups apply the parent first, retries may not.
			must(t, syncer.ApplyPaths(t.Context(), []string{child}))
			must(t, syncer.ApplyPaths(t.Context(), []string{replaced, child}))

			info, err := os.Lstat(filepath.Join(mirrored, "dir"))
			if err != nil || info.IsDir() {
				t.Fatalf("mirror kept the replaced directory: %v %v", info, err)
			}

			//nolint:gosec // Read only the mirror entry in this test's private repository.
			content, err := os.ReadFile(filepath.Join(mirrored, "other", "child"))
			if err != nil || string(content) != "other" {
				t.Fatalf("removal followed the mirrored link: %q %v", content, err)
			}
		})
	}
}
