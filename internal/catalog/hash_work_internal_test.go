package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/integrity"
)

func TestHashRefreshWorkOnlyForFullGitScans(t *testing.T) {
	t.Parallel()
	checkHashRefresh(t, true)
}

func TestInventoryScansDoNotQueueGitWork(t *testing.T) {
	t.Parallel()
	checkHashRefresh(t, false)
}

func checkHashRefresh(t *testing.T, copied bool) {
	t.Helper()

	store := testStore(t)
	if copied {
		store.BindCopiedHashes(func(integrity.Record, string) (string, int64, bool) { return "", 0, false })
	}

	scanner := integrity.NewScanner(1, 1)
	root := t.TempDir()
	path := filepath.Join(root, "file")

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	for index, full := range []bool{true, true, false} {
		writeHashFixture(t, path, string(rune('a'+index)))

		result, scanErr := store.Reconcile(t.Context(), scanner, []string{root}, matcher, full, "test", "")
		if scanErr != nil {
			t.Fatal(scanErr)
		}

		checkHashWork(t, store, result.ID, path, copied && index == 1)
	}

	writeHashFixture(t, path, "event")

	result, err := store.Observe(t.Context(), scanner, []string{path}, []string{root}, matcher, "test", "event")
	if err != nil {
		t.Fatal(err)
	}

	checkHashWork(t, store, result.ID, path, false)
}

func writeHashFixture(t *testing.T, path, content string) {
	t.Helper()

	err := os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func checkHashWork(t *testing.T, store *Store, id, path string, want bool) {
	t.Helper()

	var paths []string

	err := store.ChangedHashes(t.Context(), id, func(path string) error {
		paths = append(paths, path)

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if want && (len(paths) != 1 || paths[0] != path) || !want && len(paths) != 0 {
		t.Fatalf("refresh work = %q, expected file=%v", paths, want)
	}
}
