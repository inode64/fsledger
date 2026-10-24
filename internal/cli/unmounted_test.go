package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/resource"
)

// rememberLost records the source as a mount point; a plain directory is in no mount table, as after an
// unmount.
func rememberLost(t *testing.T, root, source string) {
	t.Helper()

	cfg, err := config.Load(filepath.Join(root, fixtureMainYaml))
	if err != nil {
		t.Fatal(err)
	}

	store, err := catalog.OpenRepository(t.Context(), cfg, fixtureRepository)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(store)

	err = store.RememberMounts(t.Context(), []string{source})
	if err != nil {
		t.Fatal(err)
	}
}

// verify rescans every source, so it refuses while a recorded filesystem is not mounted: the scan would skip
// its tree. --forget-unmounted drops that record and records what is there now, deletions included.
func TestVerifyRefusesWhileARecordedFilesystemIsNotMounted(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	file, run := inventorySource(t, root, "{}")
	rememberLost(t, root, filepath.Dir(file))

	err := os.Remove(file)
	if err != nil {
		t.Fatal(err)
	}

	_, err = run(verifyCommand)
	if err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Fatal("verify ran over a filesystem that is not mounted", err)
	}

	if changed := verifiedChanges(t, func(args ...string) (string, error) {
		return run(append(args, "--forget-unmounted")...)
	}); changed != 1 {
		t.Fatalf("forgetting the filesystem recorded %d changes, want the deletion", changed)
	}

	if changed := verifiedChanges(t, run); changed != 0 {
		t.Fatalf("the forgotten filesystem still blocks verify or records %d changes", changed)
	}
}
