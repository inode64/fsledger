package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/integrity"
)

func TestReplacedParentDirectoryObservesChildrenAsDeleted(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	root := t.TempDir()
	replaced := filepath.Join(root, "dir")
	child := filepath.Join(replaced, "child")

	err := os.Mkdir(replaced, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(child, []byte("initial"), 0o600)
	if err != nil {
		t.Fatal(err)
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

	err = os.RemoveAll(replaced)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(replaced, []byte("now a file"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	result, err := store.Observe(
		t.Context(), scanner, []string{replaced, child}, []string{root}, matcher, "actor", "replaced-parent",
	)
	if err != nil || result.Changed == 0 {
		t.Fatal("replaced parent failed the group", result, err)
	}

	_, exists, err := store.Current(t.Context(), []byte(child))
	if err != nil || exists {
		t.Fatal("replaced parent kept obsolete child", err)
	}
}
