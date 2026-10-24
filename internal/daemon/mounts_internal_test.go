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
	"github.com/inode64/fsledger/internal/mountinfo"
	"github.com/inode64/fsledger/internal/resource"
)

// mountNested binds source onto path, a directory it creates, for tests that opted in, and unmounts it at
// cleanup unless the test already did. With source "" it mounts a tmpfs instead.
func mountNested(t *testing.T, source, path string) {
	t.Helper()

	if os.Getenv("FSLEDGER_KERNEL_MOUNT") != "1" {
		t.Skip("opt in with FSLEDGER_KERNEL_MOUNT=1; mounts a tmpfs, as root or under unshare -rm")
	}

	err := os.MkdirAll(path, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	if source == "" {
		err = unix.Mount("fsledger-test", path, "tmpfs", 0, "")
	} else {
		err = unix.Mount(source, path, "", unix.MS_BIND, "")
	}

	if err != nil {
		t.Skipf("cannot mount: %v", err)
	}

	t.Cleanup(func() {
		// EINVAL: the test already unmounted it.
		unmountErr := unix.Unmount(path, 0)
		if unmountErr != nil && !errors.Is(unmountErr, unix.EINVAL) {
			t.Error(unmountErr)
		}
	})
}

// persistentMount skips a test whose bound directory lives on a volatile filesystem, which is not recorded.
func persistentMount(t *testing.T, path string) {
	t.Helper()

	table, err := mountinfo.Read()
	if err != nil {
		t.Fatal(err)
	}

	for _, mount := range mountinfo.Resolve(path, table) {
		if mount.Point == path && !mountinfo.Recordable(mount, table) {
			t.Skipf("%s is %s; set TMPDIR on a persistent filesystem", path, mount.Type)
		}
	}
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
	data := t.TempDir()
	mountNested(t, data, nested)
	persistentMount(t, nested)

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

	mountNested(t, data, nested)

	err = runner.reconcile(t.Context(), "mounted again")
	if err != nil || len(runner.status.UnmountedPaths) != 0 {
		t.Fatal("the filesystem mounted again stayed retained", runner.status.UnmountedPaths, err)
	}

	keptTree(t, runner, stats.Entries, file)
}

// lostRecords records plain directories as mount points: none is in the mount table, as after an unmount.
func lostRecords(points ...string) []mountinfo.Record {
	records := make([]mountinfo.Record, 0, len(points))
	for _, point := range points {
		records = append(records, mountinfo.Record{Point: point, Type: "ext4", Root: "/", Source: "/dev/sdb1", FSID: 0})
	}

	return records
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

	err = errors.Join(runner.catalog.RememberMounts(t.Context(), lostRecords(runner.roots...)), os.Remove(file))
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

	err = errors.Join(
		runner.catalog.RememberMounts(t.Context(), lostRecords(filepath.Dir(source))),
		os.RemoveAll(source),
	)
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

// A volatile filesystem loses its contents with the mount, so its unmount is a deletion, not a lost tree.
func TestKernelUnmountedVolatileFilesystemIsNotKept(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	nested := filepath.Join(runner.roots[0], "scratch")
	mountNested(t, "", nested)

	err := runner.reconcile(t.Context(), "mounted")
	if err != nil {
		t.Fatal(err)
	}

	if slices.Contains(mountinfo.Points(runner.mounts), nested) {
		t.Fatal("a tmpfs was recorded", runner.mounts)
	}
}

// An unmounted nested filesystem whose mount point is gone too was retired: its record goes, and its tree
// is no longer kept.
func TestRetiredNestedFilesystemIsForgotten(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	nested := filepath.Join(runner.roots[0], "volume")

	err := os.Mkdir(nested, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	runner.mounts = lostRecords(nested)
	runner.recordMounts(t.Context())

	if !slices.Equal(runner.status.UnmountedPaths, []string{nested}) {
		t.Fatal("an unmounted nested filesystem was not kept", runner.status.UnmountedPaths)
	}

	err = os.Remove(nested)
	if err != nil {
		t.Fatal(err)
	}

	runner.recordMounts(t.Context())

	if len(runner.status.UnmountedPaths) != 0 || len(runner.catalog.Mounts()) != 0 {
		t.Fatal("a retired nested filesystem was kept", runner.status.UnmountedPaths, runner.catalog.Mounts())
	}
}
