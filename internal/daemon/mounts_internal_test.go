package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/mirror"
	"github.com/inode64/fsledger/internal/mountinfo"
	"github.com/inode64/fsledger/internal/resource"
	"github.com/inode64/fsledger/internal/watcher"
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
// repository waits, the catalog, the mirror and the history keep the tree, status lists the mount point, and
// the repository resumes once it is mounted again.
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

	waited(t, runner, runner.reconcile(t.Context(), "unmounted"), nested)
	keptTree(t, runner, stats.Entries, file)

	mountNested(t, data, nested)

	err = runner.reconcile(t.Context(), "mounted again")
	if err != nil || len(runner.status.UnmountedPaths) != 0 {
		t.Fatal("the repository still waits for the filesystem mounted again", runner.status.UnmountedPaths, err)
	}

	keptTree(t, runner, stats.Entries, file)
}

// waited fails unless the operation waited for the filesystem at point and status lists it. A plain
// directory recorded as a mount point stands for one that is no longer mounted.
func waited(t *testing.T, runner *worker, err error, point string) {
	t.Helper()

	if !errors.Is(err, errFilesystemMissing) || !slices.Equal(runner.status.UnmountedPaths, []string{point}) {
		t.Fatal("the repository did not wait for the missing filesystem", runner.status.UnmountedPaths, err)
	}
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
// recorded mount point makes the repository wait instead of recording the tree as deleted.
func TestRestartOnAnUnmountedSourceKeepsItsTree(t *testing.T) {
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

	err = errors.Join(runner.catalog.RememberMounts(t.Context(), []string{source}), os.Remove(file))
	if err != nil {
		t.Fatal(err)
	}

	restart(t, runner)
	waited(t, runner, runner.reconcile(t.Context(), "restarted"), source)
	keptTree(t, runner, stats.Entries, file)
}

// A source below a recorded mount point that is no longer mounted is missing after a restart: it was lost
// with its filesystem, so startup keeps it and the repository waits instead of pruning its tree.
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
		runner.catalog.RememberMounts(t.Context(), []string{filepath.Dir(source)}),
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

	waited(t, runner, runner.reconcile(t.Context(), "restarted"), filepath.Dir(source))
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

	if slices.Contains(runner.mounts, nested) {
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

	runner.mounts = []string{nested}

	err = runner.recordMounts(t.Context())
	if err != nil || !slices.Equal(runner.status.UnmountedPaths, []string{nested}) {
		t.Fatal("an unmounted nested filesystem was not kept", runner.status.UnmountedPaths, err)
	}

	err = errors.Join(os.Remove(nested), runner.recordMounts(t.Context()))
	if err != nil {
		t.Fatal(err)
	}

	if len(runner.status.UnmountedPaths) != 0 || len(runner.catalog.Mounts()) != 0 {
		t.Fatal("a retired nested filesystem was kept", runner.status.UnmountedPaths, runner.catalog.Mounts())
	}
}

// An event queued before an unmount finds its path missing because it went with the filesystem, not because
// it was deleted: the commit waits, and the catalog, the mirror and the history keep the path.
func TestEventBelowAnUnmountedFilesystemDeletesNothing(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	nested := filepath.Join(runner.roots[0], "data")

	err := os.Mkdir(nested, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	file := writeSource(t, runner, "data/file", "kept")

	err = runner.reconcile(t.Context(), "mounted")
	if err != nil {
		t.Fatal(err)
	}

	stats, err := runner.catalog.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	runner.mounts = []string{nested}

	err = os.Remove(file)
	if err != nil {
		t.Fatal(err)
	}

	waited(t, runner, commitGroup(t, runner, file), nested)
	keptTree(t, runner, stats.Entries, file)
}

// A filesystem mounted after the last reconciliation is recorded by the next commit, so its unmount before
// the following reconciliation makes the repository wait instead of deleting its tree.
func TestKernelMountBetweenReconciliationsIsRecorded(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)

	err := runner.reconcile(t.Context(), "before the mount")
	if err != nil {
		t.Fatal(err)
	}

	nested := filepath.Join(runner.roots[0], "data")
	mountNested(t, t.TempDir(), nested)
	persistentMount(t, nested)

	file := writeSource(t, runner, "data/file", "nested")
	commitEvent(t, runner, file, event.Actor{})

	stats, err := runner.catalog.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	err = unix.Unmount(nested, 0)
	if err != nil {
		t.Fatal(err)
	}

	waited(t, runner, commitGroup(t, runner, file), nested)
	keptTree(t, runner, stats.Entries, file)
}

// commitGroup commits one event for path, as the flush loop does, and returns the commit's error.
func commitGroup(t *testing.T, runner *worker, path string) error {
	t.Helper()

	group := event.Group{Actor: event.Actor{}, Paths: map[string]event.Raw{path: {Path: path}}}

	return runner.commit(t.Context(), group)
}

// A source that is itself a mount point waits while unmounted, after a restart too, and its empty mount
// point is not pinned meanwhile: once the filesystem is mounted again the repository resumes instead of
// reporting it changed.
func TestKernelUnmountedSourceResumesWhenMountedAgain(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	source, data := runner.roots[0], t.TempDir()
	mountNested(t, data, source)
	persistentMount(t, source)

	file := writeSource(t, runner, "file", "kept")

	err := runner.reconcile(t.Context(), "mounted")
	if err != nil {
		t.Fatal(err)
	}

	stats, err := runner.catalog.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	err = unix.Unmount(source, 0)
	if err != nil {
		t.Fatal(err)
	}

	restart(t, runner)
	waited(t, runner, runner.reconcile(t.Context(), "unmounted"), source)

	mountNested(t, data, source)

	err = runner.reconcile(t.Context(), "mounted again")
	if err != nil {
		t.Fatal("the source mounted again did not resume", err)
	}

	keptTree(t, runner, stats.Entries, file)
}

// systemd binds a ReadWritePaths directory onto itself in the service's namespace, also when the volume
// meant to be there is missing: that bind is the empty mount point, so the repository waits.
func TestKernelSelfBindAtAMissingPointWaits(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	nested := filepath.Join(runner.roots[0], "data")
	mountNested(t, t.TempDir(), nested)
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

	mountNested(t, nested, nested)
	waited(t, runner, runner.reconcile(t.Context(), "bound onto itself"), nested)
	keptTree(t, runner, stats.Entries, file)
}

// A recorded filesystem mounted over a watched source, for the first time or once the repository waited for
// it, replaces what its watchers and its pinned identity follow: the watchers restart, and the repository
// records what is mounted instead of reporting the source changed.
func TestKernelFilesystemMountedOverASourceRestartsItsWatchers(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	runner.cfg.Watch.Backend = event.Inotify
	runner.queue = watcher.NewQueue(8)
	source, data := runner.roots[0], t.TempDir()

	err := runner.reconcile(t.Context(), "before the mount")
	if err != nil {
		t.Fatal(err)
	}

	runner.startWatchers(t.Context())
	defer runner.stopWatching()

	adopt := func(name string) {
		t.Helper()

		watchers := slices.Clone(runner.watchers)

		mountNested(t, data, source)
		persistentMount(t, source)
		file := writeSource(t, runner, name, name)

		runner.dirty = true
		runner.flushReconciliation(t.Context(), time.Now())

		if runner.dirty || slices.Equal(watchers, runner.watchers) ||
			!strings.Contains(gitOutput(t, runner, "ls-files"), strings.TrimPrefix(file, "/")) {
			t.Fatal("the filesystem mounted over the source was not adopted", runner.errorReasons, runner.watchers)
		}
	}

	adopt("first")

	err = unix.Unmount(source, 0)
	if err != nil {
		t.Fatal(err)
	}

	waited(t, runner, runner.reconcile(t.Context(), "unmounted"), source)
	adopt("again")
}

// A source on a recorded filesystem of its own, inside another recorded one, waits once unmounted: the outer
// filesystem now holding its mount point was not mounted over it, so its watchers and identity stay.
func TestKernelUnmountedSourceInsideARecordedFilesystemKeepsItsWatchers(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	runner.cfg.Watch.Backend = event.Inotify
	runner.queue = watcher.NewQueue(8)
	source := runner.roots[0]
	mountNested(t, t.TempDir(), source)
	persistentMount(t, source)

	table, err := mountinfo.Read()
	if err != nil {
		t.Fatal(err)
	}

	outer := mountinfo.Resolve(filepath.Dir(source), table)
	if len(outer) == 0 {
		t.Fatal("no filesystem holds", source)
	}

	// The filesystem holding the mount point stands for a recorded outer one.
	runner.mounts = []string{outer[0].Point}

	err = runner.reconcile(t.Context(), "mounted")
	if err != nil {
		t.Fatal(err)
	}

	runner.startWatchers(t.Context())
	defer runner.stopWatching()

	err = unix.Unmount(source, 0)
	if err != nil {
		t.Fatal(err)
	}

	waited(t, runner, runner.reconcile(t.Context(), "unmounted"), source)

	if runner.sourcesPending || len(runner.returnedRoots) != 0 {
		t.Fatal("the watchers of a source waiting for its filesystem were restarted", runner.returnedRoots)
	}
}

// Events queued on the directory a filesystem is then mounted over describe what it covers: their actor must
// not sign the mounted bytes, which the reconciliation records instead.
func TestKernelMountOverASourceDropsTheActorsOfQueuedEvents(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	runner.cfg.Watch.Backend = event.Inotify
	runner.queue = watcher.NewQueue(8)
	source, data := runner.roots[0], t.TempDir()
	file := writeSource(t, runner, "config", "covered")

	err := runner.reconcile(t.Context(), "before the mount")
	if err != nil {
		t.Fatal(err)
	}

	runner.startWatchers(t.Context())
	defer runner.stopWatching()

	writeSource(t, runner, "config", "written on the covered directory")

	err = os.WriteFile(filepath.Join(data, "config"), []byte("mounted"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mountNested(t, data, source)
	persistentMount(t, source)

	now := time.Now()
	actor := event.Actor{
		PID:        10,
		StartTime:  100,
		UID:        4242,
		LoginUID:   4242,
		UserKnown:  true,
		LoginKnown: true,
		Known:      true,
	}
	runner.accept(event.Raw{Path: file, Actor: actor, Backend: event.Inotify, Time: now})
	runner.flush(t.Context(), now.Add(time.Hour))

	relative := strings.TrimPrefix(file, "/")
	if strings.Contains(gitOutput(t, runner, "log", "--format=%s%n%b", "--", relative), "4242") ||
		gitOutput(t, runner, "show", "HEAD:"+relative) != "mounted" {
		t.Fatal("the mounted bytes were not recorded without the queued actor", runner.errorReasons)
	}
}
