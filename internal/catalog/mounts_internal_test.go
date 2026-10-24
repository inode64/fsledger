package catalog

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/inode64/fsledger/internal/config"
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
