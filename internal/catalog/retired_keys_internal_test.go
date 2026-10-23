package catalog

import (
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/config"
)

// Catalogs written before the baseline was removed lose its keys when opened, and nothing else.
func TestOpenDropsRetiredKeys(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), config.CatalogDirectory)
	store := testStoreAt(t, path)
	observeInitial(t, store, workloadRecord(0))

	transaction := store.begin()
	for _, lookup := range []string{"b/0/etc", "b/1/etc", "p/etc", "t/token", "a/etc"} {
		transaction.set([]byte(lookup), []byte("retired"))
	}

	err := transaction.commit()
	if err != nil {
		t.Fatal(err)
	}

	reopened := reopenTestStore(t, store, path, store.policy)

	for _, prefix := range []string{"b/", "p/", "t/", "a/"} {
		err = reopened.iterate(t.Context(), prefix, func(key, _ []byte) error {
			t.Errorf("retired key %s%s kept", prefix, key)

			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	_, exists, err := reopened.Current(t.Context(), workloadRecord(0).Path)
	if err != nil || !exists {
		t.Fatal("current record lost with the retired keys", err)
	}
}
