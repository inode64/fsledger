package mirror

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

func directorySyncMirror(t *testing.T) (*Sync, string, string) {
	t.Helper()
	source := t.TempDir()

	writeUnstableFixture(t, filepath.Join(source, "directory", "first"), "first")
	writeUnstableFixture(t, filepath.Join(source, "directory", "second"), "second")
	writeUnstableFixture(t, filepath.Join(source, "directory", "nested", "third"), "third")

	err := os.Symlink("directory", filepath.Join(source, "link"))
	if err != nil {
		t.Fatal(err)
	}

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	syncer, err := Open(t.TempDir(), []string{source}, matcher, "sha256")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.Close(syncer) })

	relative, err := Relative(source)
	if err != nil {
		t.Fatal(err)
	}

	return syncer, source, relative
}

// recordDirectorySyncs counts each mirror directory synced, still syncing it for real.
func recordDirectorySyncs(syncer *Sync) map[string]int {
	synced := make(map[string]int)
	syncer.syncDirectory = func(root *os.Root, name string) error {
		synced[name]++

		return syncDirectory(root, name)
	}

	return synced
}

func checkDirectorySyncs(t *testing.T, synced map[string]int, want ...string) {
	t.Helper()

	expected := make(map[string]int, len(want))
	for _, name := range want {
		expected[name] = 1
	}

	if !maps.Equal(synced, expected) {
		t.Fatalf("synced directories %v, want each of %q once", synced, want)
	}

	clear(synced)
}

func TestReconciliationSyncsEachTouchedDirectoryOnce(t *testing.T) {
	t.Parallel()

	syncer, _, relative := directorySyncMirror(t)
	synced := recordDirectorySyncs(syncer)

	err := syncer.ReconcileContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// Every mirror ancestor was created, the link and copies entered their parents, and the copied
	// temporaries left the root.
	want := []string{"."}
	for name := filepath.Join(relative, "directory", "nested"); name != "."; name = filepath.Dir(name) {
		want = append(want, name)
	}

	checkDirectorySyncs(t, synced, want...)
}

func TestEventGroupSyncsChangedDirectoriesOnce(t *testing.T) {
	t.Parallel()

	syncer, source, relative := directorySyncMirror(t)

	err := syncer.ReconcileContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	synced := recordDirectorySyncs(syncer)
	directory := filepath.Join(source, "directory")
	writeUnstableFixture(t, filepath.Join(directory, "first"), "changed")

	for _, name := range []string{"second", "nested"} {
		err = os.RemoveAll(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
	}

	err = syncer.ApplyPaths(t.Context(), []string{
		filepath.Join(directory, "first"), filepath.Join(directory, "nested"), filepath.Join(directory, "second"),
	})
	if err != nil {
		t.Fatal(err)
	}

	checkDirectorySyncs(t, synced, ".", filepath.Join(relative, "directory"))
}

func TestRefreshDefersSyncAndRemovalForgetsDirectory(t *testing.T) {
	t.Parallel()

	syncer, source, relative := directorySyncMirror(t)

	err := syncer.ReconcileContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	synced := recordDirectorySyncs(syncer)
	directory := filepath.Join(source, "directory")

	err = syncer.RefreshContext(t.Context(), filepath.Join(directory, "nested", "third"))
	if err != nil {
		t.Fatal(err)
	}

	checkDirectorySyncs(t, synced)

	err = os.RemoveAll(directory)
	if err != nil {
		t.Fatal(err)
	}

	err = syncer.ApplyPaths(t.Context(), []string{directory})
	if err != nil {
		t.Fatal("removed directory still pending sync", err)
	}

	// The refreshed copy's directory went with its parent, whose removal its own parent records.
	checkDirectorySyncs(t, synced, ".", relative)
}

func TestDirectorySyncFailureFailsMirrorOperation(t *testing.T) {
	t.Parallel()

	syncer, source, _ := directorySyncMirror(t)
	failure := fault.New("directory sync failed")
	syncer.syncDirectory = func(*os.Root, string) error { return failure }

	err := syncer.ReconcileContext(t.Context())
	if !errors.Is(err, failure) {
		t.Fatal("reconciliation ignored a failed directory sync", err)
	}

	first := filepath.Join(source, "directory", "first")
	writeUnstableFixture(t, first, "changed")

	err = syncer.ApplyPaths(t.Context(), []string{first})
	if !errors.Is(err, failure) {
		t.Fatal("event group ignored a failed directory sync", err)
	}
}

// The copies stay cached after a failed directory sync, so the next pass touches nothing: the directories
// must stay pending, or a later commit records entries that a crash can still lose.
func TestFailedDirectorySyncIsRetried(t *testing.T) {
	t.Parallel()

	syncer, _, relative := directorySyncMirror(t)
	failure := fault.New("directory sync failed")
	syncer.syncDirectory = func(*os.Root, string) error { return failure }

	err := syncer.ReconcileContext(t.Context())
	if !errors.Is(err, failure) {
		t.Fatal("reconciliation ignored a failed directory sync", err)
	}

	synced := recordDirectorySyncs(syncer)

	err = syncer.ReconcileContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"."}
	for name := filepath.Join(relative, "directory", "nested"); name != "."; name = filepath.Dir(name) {
		want = append(want, name)
	}

	checkDirectorySyncs(t, synced, want...)
}
