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

		_, scanErr := store.Reconcile(t.Context(), scanner, []string{root}, matcher, full, "test", "")
		if scanErr != nil {
			t.Fatal(scanErr)
		}

		checkHashWork(t, store, path, copied && index == 1)
		clearHashWork(t, store)
	}

	writeHashFixture(t, path, "event")

	_, err = store.Observe(t.Context(), scanner, []string{path}, []string{root}, matcher, "test", "event")
	if err != nil {
		t.Fatal(err)
	}

	checkHashWork(t, store, path, false)
}

// The refresh consumes its work only once the mirror copy and its commit succeed: work left by a failed
// refresh, or by a daemon that died before it, outlives the next reconciliation instead of being lost.
func TestUnconsumedHashRefreshWorkSurvivesTheNextReconciliation(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	store.BindCopiedHashes(func(integrity.Record, string) (string, int64, bool) { return "", 0, false })

	scanner := integrity.NewScanner(1, 1)
	root := t.TempDir()
	path := filepath.Join(root, "file")

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	for _, content := range []string{"first", "second", "second"} {
		writeHashFixture(t, path, content)

		_, err = store.Reconcile(t.Context(), scanner, []string{root}, matcher, true, "test", "")
		if err != nil {
			t.Fatal(err)
		}
	}

	checkHashWork(t, store, path, true)
}

func clearHashWork(t *testing.T, store *Store) {
	t.Helper()

	err := store.ClearOperation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
}

func writeHashFixture(t *testing.T, path, content string) {
	t.Helper()

	err := os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func checkHashWork(t *testing.T, store *Store, path string, want bool) {
	t.Helper()

	var paths []string

	err := store.ChangedHashes(t.Context(), func(path string) error {
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

func TestFullScanCountsVerifiedMirrorDigests(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	root := t.TempDir()
	path := filepath.Join(root, "file")
	writeHashFixture(t, path, "content")

	scanner := integrity.NewScanner(1, 1)

	record, err := scanner.Observe(t.Context(), path, "sha256", true, nil)
	if err != nil {
		t.Fatal(err)
	}

	store.BindCopiedHashes(func(integrity.Record, string) (string, int64, bool) {
		return record.Hash, record.HashedAt, true
	})

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	result, err := store.Reconcile(t.Context(), scanner, []string{root}, matcher, true, "mirror", "")
	if err != nil || result.Hashed != 1 {
		t.Fatal("verified mirror digest missing from full-scan count", result, err)
	}
}

// An import drops the refreshes of the paths it wrote, not those its full scan found elsewhere: a file the
// import never touched may have changed behind an unchanged signature, and only its forced copy brings the
// new bytes to the mirror.
func TestDroppedRefreshesAreOnlyTheImportedPaths(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	store.BindCopiedHashes(func(integrity.Record, string) (string, int64, bool) { return "", 0, false })

	scanner := integrity.NewScanner(1, 1)
	root := t.TempDir()
	imported, local := filepath.Join(root, "imported"), filepath.Join(root, "local")

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	var result Result

	for _, content := range []string{"before", "after"} {
		writeHashFixture(t, imported, content)
		writeHashFixture(t, local, content)

		result, err = store.Reconcile(t.Context(), scanner, []string{root}, matcher, true, "test", "")
		if err != nil {
			t.Fatal(err)
		}
	}

	err = store.DropRefreshes(t.Context(), result.ID, [][]byte{[]byte(imported)})
	if err != nil {
		t.Fatal(err)
	}

	checkHashWork(t, store, local, true)
}
