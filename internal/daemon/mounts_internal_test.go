package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/mirror"
	"github.com/inode64/fsledger/internal/resource"
)

// mountNested mounts a private tmpfs on a new directory, for tests that opted in, and unmounts it at
// cleanup unless the test already did.
func mountNested(t *testing.T, path string) {
	t.Helper()

	if os.Getenv("FSLEDGER_KERNEL_MOUNT") != "1" {
		t.Skip("opt in with FSLEDGER_KERNEL_MOUNT=1; mounts a tmpfs, as root or under unshare -rm")
	}

	err := os.MkdirAll(path, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = unix.Mount("fsledger-test", path, "tmpfs", 0, "")
	if err != nil {
		t.Skipf("cannot mount tmpfs: %v", err)
	}

	t.Cleanup(func() {
		// EINVAL: the test already unmounted it.
		unmountErr := unix.Unmount(path, 0)
		if unmountErr != nil && !errors.Is(unmountErr, unix.EINVAL) {
			t.Error(unmountErr)
		}
	})
}

// keptTree fails unless the catalog still counts entries, and the mirror and HEAD still hold path.
func keptTree(t *testing.T, runner *worker, entries int64, path string) {
	t.Helper()

	stats, err := runner.catalog.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	relative := strings.TrimPrefix(path, "/")

	_, err = os.Lstat(filepath.Join(runner.repo.Path, relative))
	if err != nil || stats.Entries != entries || !strings.Contains(gitOutput(t, runner, "ls-files"), relative) {
		t.Fatal("the tree of an unmounted filesystem was recorded as deleted", stats.Entries, entries, err)
	}
}

// Unmounting a filesystem mounted inside a source loses its tree with it rather than deleting it: the
// catalog, the mirror and the history keep it, status lists it, and the rest of the source carries on.
func TestKernelUnmountedNestedFilesystemKeepsItsTree(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	nested := filepath.Join(runner.roots[0], "data")
	mountNested(t, nested)

	file := writeSource(t, runner, "data/file", "nested")

	err := runner.reconcile(t.Context(), "mounted")
	if err != nil {
		t.Fatal(err)
	}

	stats, err := runner.catalog.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	err = unix.Unmount(nested, 0)
	if err != nil {
		t.Fatal(err)
	}

	err = runner.reconcile(t.Context(), "unmounted")
	if err != nil {
		t.Fatal(err)
	}

	keptTree(t, runner, stats.Entries, file)

	if !slices.Equal(runner.status.UnmountedPaths, []string{nested}) {
		t.Fatal("status does not list the unmounted filesystem", runner.status.UnmountedPaths)
	}

	mountNested(t, nested)
	writeSource(t, runner, "data/file", "nested")

	err = runner.reconcile(t.Context(), "mounted again")
	if err != nil || len(runner.status.UnmountedPaths) != 0 {
		t.Fatal("the filesystem mounted again stayed retained", runner.status.UnmountedPaths, err)
	}

	keptTree(t, runner, stats.Entries, file)
}

// restart drops what the process pinned, as a new daemon starts with only the catalog's records.
func restart(t *testing.T, runner *worker) {
	t.Helper()

	syncer, err := mirror.Open(runner.repo.Path, runner.roots, runner.matcher, "sha256")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.Close(syncer) })

	runner.scanner, runner.mirror = integrity.NewScanner(1, 1), syncer
	runner.restoreMounts()
}

// After a restart with the source's filesystem unmounted, the source found is its empty mount point: the
// recorded mount point makes it unavailable instead of recording its tree as deleted. A plain directory
// recorded as a mount point stands for one that is no longer mounted.
func TestRestartOnAnUnmountedSourceKeepsItsTree(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	file := writeSource(t, runner, "file", "kept")

	err := runner.reconcile(t.Context(), "mounted")
	if err != nil {
		t.Fatal(err)
	}

	stats, err := runner.catalog.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	err = errors.Join(runner.catalog.RememberMounts(t.Context(), runner.roots), os.Remove(file))
	if err != nil {
		t.Fatal(err)
	}

	restart(t, runner)

	err = runner.reconcile(t.Context(), "restarted")
	if !errors.Is(err, integrity.ErrUnavailable) {
		t.Fatal("the empty mount point was taken for the source", err)
	}

	keptTree(t, runner, stats.Entries, file)
}

// A source below a recorded mount point that is no longer mounted is missing after a restart: it was lost
// with its filesystem, so startup keeps it unavailable instead of pruning its tree.
func TestRestartKeepsASourceMissingBelowAnUnmountedMountPoint(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	file := writeSource(t, runner, "file", "kept")
	source := runner.roots[0]

	err := runner.reconcile(t.Context(), "mounted")
	if err != nil {
		t.Fatal(err)
	}

	stats, err := runner.catalog.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	err = errors.Join(runner.catalog.RememberMounts(t.Context(), []string{filepath.Dir(source)}), os.RemoveAll(source))
	if err != nil {
		t.Fatal(err)
	}

	restart(t, runner)
	// The selection no longer finds the source.
	runner.roots = nil

	err = runner.retainRecorded(t.Context(), false)
	if err != nil || !slices.Equal(runner.roots, []string{source}) ||
		!slices.Equal(runner.status.UnavailableSources, []string{source}) {
		t.Fatal("the lost source was not kept", runner.roots, runner.status.UnavailableSources, err)
	}

	err = runner.reconcile(t.Context(), "restarted")
	if !errors.Is(err, integrity.ErrUnavailable) {
		t.Fatal("the lost source was reconciled", err)
	}

	keptTree(t, runner, stats.Entries, file)
}
