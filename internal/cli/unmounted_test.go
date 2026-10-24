package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/resource"
)

// withFixtureStore lets use read or change the fixture repository's catalog, closed again before verify runs.
func withFixtureStore(t *testing.T, root string, use func(*catalog.Store)) {
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

	use(store)
}

// rememberLost records points as mount points; a plain directory is in no mount table, as after an unmount.
func rememberLost(t *testing.T, root string, points ...string) {
	t.Helper()

	withFixtureStore(t, root, func(store *catalog.Store) {
		err := store.RememberMounts(t.Context(), points)
		if err != nil {
			t.Fatal(err)
		}
	})
}

// verify rescans every source, so it refuses while a recorded filesystem is not mounted: its tree would read
// as deleted. --forget-unmounted drops that record, lists it, and records what is there now, deletions
// included.
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

	var output string

	if changed := verifiedChanges(t, func(args ...string) (string, error) {
		var runErr error

		output, runErr = run(append(args, "--forget-unmounted")...)

		return output, runErr
	}); changed != 1 {
		t.Fatalf("forgetting the filesystem recorded %d changes, want the deletion", changed)
	}

	var report struct {
		Forgotten []string `json:"forgotten_mounts"`
	}

	err = json.Unmarshal([]byte(output), &report)
	if err != nil || !slices.Equal(report.Forgotten, []string{filepath.Dir(file)}) {
		t.Fatal("the forgotten mount point was not listed", output, err)
	}

	if changed := verifiedChanges(t, run); changed != 0 {
		t.Fatalf("the forgotten filesystem still blocks verify or records %d changes", changed)
	}
}

// verify drops the trees recorded outside the selection, so a source missing below a recorded point that is
// not mounted makes it refuse too: the source was lost with that filesystem.
func TestVerifyRefusesWhileALostSourceWaitsForItsFilesystem(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	file, run := inventorySource(t, root, "{}")
	rememberLost(t, root, root)

	err := os.RemoveAll(filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}

	_, err = run(verifyCommand)
	if err == nil || !strings.Contains(err.Error(), "not mounted at "+root) {
		t.Fatal("verify dropped a source lost with its filesystem", err)
	}
}

// A recorded point that holds no source and lies inside none is stale: verify neither waits for it nor keeps it.
func TestVerifyForgetsMountPointsOfNoSource(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	_, run := inventorySource(t, root, "{}")
	rememberLost(t, root, filepath.Join(root, "elsewhere"))

	if changed := verifiedChanges(t, run); changed != 0 {
		t.Fatalf("a mount point of no source recorded %d changes", changed)
	}

	withFixtureStore(t, root, func(store *catalog.Store) {
		if mounts := store.Mounts(); len(mounts) != 0 {
			t.Fatal("a mount point of no source was kept", mounts)
		}
	})
}
