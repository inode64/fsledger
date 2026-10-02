package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/integrity"
)

//nolint:funlen // Ordered integration fixture keeps deletion, approval and generation assertions together.
func TestDeletionPreservesOperationAndDoesNotScanRoots(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	root := t.TempDir()
	removed := filepath.Join(root, "gone")

	err := os.Mkdir(removed, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{filepath.Join(removed, "file"), filepath.Join(root, "changed")} {
		err = os.WriteFile(path, []byte("initial"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	scanner := integrity.NewScanner(1, 2)

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Reconcile(t.Context(), scanner, []string{root}, matcher, true, "initial", "")
	if err != nil {
		t.Fatal(err)
	}

	err = store.InitBaseline(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	generation := store.state.Generation

	err = os.RemoveAll(removed)
	if err != nil {
		t.Fatal(err)
	}

	changed := filepath.Join(root, "changed")

	err = os.WriteFile(changed, []byte("changed"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	result, err := store.Observe(
		t.Context(),
		scanner,
		[]string{changed, removed},
		[]string{root},
		matcher,
		"actor",
		"git-trailer-id",
	)
	if err != nil || result.ID != "git-trailer-id" || store.state.Generation != generation {
		t.Fatal("deletion rescanned or changed ID", result, err)
	}

	err = store.Accept(t.Context(), result.ID)
	if err != nil {
		t.Fatal("Git operation cannot be approved", err)
	}

	if ids := pendingIDs(t, store); len(ids) != 0 {
		t.Fatal("subtree deletion not approved", ids)
	}
}
