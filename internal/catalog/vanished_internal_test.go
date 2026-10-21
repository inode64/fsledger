package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/integrity"
)

// Deferred hashing observes a new file twice; a deletion in between is a deletion, not a failure.
func TestObserveNewFileVanishedBeforeHash(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	store.Policy.Hash.OnEvent = false
	root := t.TempDir()
	vanished := filepath.Join(root, "vanished")
	kept := filepath.Join(root, "kept")

	for _, path := range []string{vanished, kept} {
		err := os.WriteFile(path, []byte("new"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	var (
		removed   bool
		removeErr error
	)

	// Only the hashing observation consults copied digests, after metadata was read.
	store.BindCopiedHashes(func(record integrity.Record, _ string) (string, int64, bool) {
		if string(record.Path) == vanished && !removed {
			removed = true
			removeErr = os.Remove(vanished)
		}

		return "", 0, false
	})

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Observe(
		t.Context(),
		integrity.NewScanner(1, 1),
		[]string{vanished, kept},
		[]string{root},
		matcher,
		"actor",
		"vanished",
	)
	if err != nil || !removed || removeErr != nil {
		t.Fatal("vanished new file failed the observation", err, removed, removeErr)
	}

	for path, want := range map[string]bool{vanished: false, kept: true} {
		_, exists, currentErr := store.Current(t.Context(), []byte(path))
		if currentErr != nil || exists != want {
			t.Fatal("unexpected inventory after vanished new file", path, exists, currentErr)
		}
	}
}
