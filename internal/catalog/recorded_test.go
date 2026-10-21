package catalog_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/resource"
)

func TestRecordedOutsideReturnsTopmostUnselectedPaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// "lost-x" sorts between "lost" and "lost/nested", which must still be found below "lost".
	roots := []string{filepath.Join(root, "kept"), filepath.Join(root, "lost"), filepath.Join(root, "lost-x")}
	for _, source := range roots {
		err := os.MkdirAll(filepath.Join(source, "nested"), 0o700)
		if err == nil {
			err = os.WriteFile(filepath.Join(source, "nested", "file"), []byte(source), 0o600)
		}

		if err != nil {
			t.Fatal(err)
		}
	}

	policy, notifications := options(t)

	store, err := catalog.Open(
		t.Context(), filepath.Join(root, "db", "catalog.pebble"), "test", "host", policy, notifications,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(store)

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Reconcile(t.Context(), integrity.NewScanner(1, 2), roots, matcher, true, "unknown", "")
	if err != nil {
		t.Fatal(err)
	}

	recorded, err := store.RecordedOutside(t.Context(), roots[:1])
	if err != nil || !slices.Equal(recorded, roots[1:]) {
		t.Fatalf("recorded sources outside the selection: %v %v", recorded, err)
	}
}
