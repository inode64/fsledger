package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/integrity"
)

// The recorded mount points outlive the process: after a restart they are the only memory of a filesystem
// that was mounted under a source.
func TestRecordedMountsSurviveReopening(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), config.CatalogDirectory)
	cfg := testStoreConfig(t)
	store := openTestStore(t, path, cfg.Integrity, cfg.Notifications)
	mounts := []string{"/srv/data", "/var/www"}

	err := store.RememberMounts(t.Context(), mounts)
	if err != nil {
		t.Fatal(err)
	}

	store = reopenTestStore(t, store, path, cfg.Integrity)

	if !slices.Equal(store.Mounts(), mounts) {
		t.Fatal("recorded mount points lost on reopening", store.Mounts())
	}
}

// A walk that ends with a recorded filesystem unmounted saw its empty mount point: it deletes nothing. A
// plain directory recorded as a mount point stands for one that is no longer mounted.
func TestReconcileDeletesNothingWhileARecordedFilesystemIsMissing(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	root := t.TempDir()
	file := filepath.Join(root, "file")
	writeHashFixture(t, file, "kept")

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	scanner := integrity.NewScanner(1, 1)

	_, err = store.Reconcile(t.Context(), scanner, []string{root}, matcher, true, "test", "")
	if err != nil {
		t.Fatal(err)
	}

	err = errors.Join(store.RememberMounts(t.Context(), []string{root}), os.Remove(file))
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Reconcile(t.Context(), scanner, []string{root}, matcher, true, "test", "")
	if !errors.Is(err, integrity.ErrUnavailable) {
		t.Fatal("a walk over a missing filesystem was completed", err)
	}

	stats, err := store.Stats(t.Context())
	if err != nil || stats.Entries == 0 {
		t.Fatal("the tree of a missing filesystem was deleted", stats.Entries, err)
	}
}
