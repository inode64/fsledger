package inotify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/mountinfo"
	"github.com/inode64/fsledger/internal/resource"
)

// The watcher answers for the mounts on its root and below it, except those it never watches: excluded ones and
// the nested mounts the daemon protects because it started detectors of their own.
func TestMountStacksKeepTheRootAndItsBoundaries(t *testing.T) {
	t.Parallel()

	const (
		root = "/srv"
		data = "/srv/data"
	)

	matcher, err := exclude.New("/srv/cache")
	if err != nil {
		t.Fatal(err)
	}

	matcher = matcher.Protect("/srv/nested")
	lower, upper, itself := mountAt(data, 30, 2), mountAt(data, 31, 40), mountAt(root, 20, 1)

	stacks := stackMounts(root, matcher, []mountinfo.Mount{
		mountAt("/", 1, 0), itself, lower, upper,
		mountAt("/srv/cache/tmp", 32, 41), mountAt("/srv/nested", 33, 3), mountAt("/srvx", 34, 4),
	})

	if len(stacks) != 2 || !slices.Equal(stacks[root], []mountinfo.Mount{itself}) ||
		!slices.Equal(stacks[data], []mountinfo.Mount{lower, upper}) {
		t.Fatal("wrong mounts followed", stacks)
	}

	if !stacks.unwatched(root) || !stacks.boundary(root, data) || stacks.boundary(root, root) ||
		!stacks.bounded(root, data+"/inner") || stacks.bounded(root, "/srv/other") {
		t.Fatal("the filesystem below the root is no boundary, or the root's own one is", stacks)
	}

	// A remount reusing the mount ID of the filesystem it replaced still changes the stack.
	replaced := mountStacks{root: {mountAt(root, 20, 9)}, "/srv/new": {upper}}
	if got := replaced.changed(stacks); !slices.Equal(got, []string{root, data, "/srv/new"}) {
		t.Fatal("wrong remounted points", got)
	}

	if got := stacks.changed(stacks); len(got) != 0 {
		t.Fatal("an unchanged table reported remounts", got)
	}

	// The root's own filesystem needs no reconciliation, unless other clients can change it unseen.
	remote := mountAt(root, 21, 42)
	remote.Type = "nfs4"

	if (mountStacks{root: {itself}}).unwatched(root) || !(mountStacks{root: {itself, remote}}).unwatched(root) {
		t.Fatal("the root's own filesystem was left to the wrong detection")
	}
}

// A filesystem mounted below the root after Start used to stay invisible: its contents raised no event, and nothing
// asked for reconciliation either. A rootless test cannot mount one, so the table is handed over directly.
func TestFilesystemMountedBelowRootIsABoundary(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mounted := filepath.Join(root, "mounted")
	inner := filepath.Join(mounted, "inner")

	err := os.Mkdir(mounted, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	detector := treeWatcher(t, root)
	boundary := mountStacks{mounted: {mountAt(mounted, 40, 40)}}
	detector.remount(boundary)

	if !detector.PollingRequired() || detector.Dirty() {
		t.Fatal("the mounted filesystem was left to neither watches nor reconciliation")
	}

	expectRemount(t, detector, mounted)

	// The mounted filesystem's own directories; a walk of the root that the root's remount causes stays out.
	err = os.Mkdir(inner, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	rootRemounted := mountStacks{root: {mountAt(root, 41, 41)}, mounted: boundary[mounted]}
	detector.remount(rootRemounted)
	expectRemount(t, detector, root)

	if slices.Contains(detector.native.WatchList(), inner) || !slices.Contains(detector.native.WatchList(), root) {
		t.Fatal(
			"watches crossed into the mounted filesystem, or the root's were not rebuilt",
			detector.native.WatchList(),
		)
	}

	detector.remount(mountStacks{root: rootRemounted[root]})
	expectRemount(t, detector, mounted)

	if !slices.Contains(detector.native.WatchList(), inner) || detector.PollingRequired() || detector.Dirty() {
		t.Fatal("the directory uncovered again has no watches, or reconciliation outlived the boundary")
	}
}

// The watches on a filesystem vanish with it. A detector rooted at a nested mount found at startup is the only one
// for that path, since the parent's matcher protects it: the directory the unmount uncovers must be watched again.
func TestUnmountedRootGetsItsWatchesBack(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	nested := filepath.Join(root, "nested")

	err := os.Mkdir(nested, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	// No watch yet stands for the ones IN_UNMOUNT removed.
	detector, err := New(root, matcher)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(detector)

	detector.mounts = mountStacks{root: {mountAt(root, 30, 2)}}
	detector.remount(mountStacks{})
	expectRemount(t, detector, root)

	if got := detector.native.WatchList(); !slices.Contains(got, root) || !slices.Contains(got, nested) {
		t.Fatal("the uncovered root was not watched again", got)
	}
}

// A filesystem unmounted and mounted again before the follower reads the table brings back the same directories,
// whose watches the kernel dropped at the unmount: they must be installed again, not taken as still watched.
func TestRemountedRootWithTheSameDirectoriesIsWatchedAgain(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	nested := filepath.Join(root, "nested")

	err := os.Mkdir(nested, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	detector := treeWatcher(t, root)

	// IN_UNMOUNT made fsnotify forget every watch on the filesystem.
	for _, name := range detector.native.WatchList() {
		err = detector.native.Remove(name)
		if err != nil {
			t.Fatal(err)
		}
	}

	detector.mounts = mountStacks{root: {mountAt(root, 30, 2)}}
	detector.remount(mountStacks{root: {mountAt(root, 31, 2)}})
	expectRemount(t, detector, root)

	if got := detector.native.WatchList(); !slices.Contains(got, root) || !slices.Contains(got, nested) {
		t.Fatal("the remounted root was not watched again", got)
	}
}

// The mount follower ends with the context, like the reader, and Close still waits for it.
func TestMountFollowerEndsWithItsContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	detector := startedWatcher(ctx, t, t.TempDir())

	cancel()

	select {
	case <-detector.mountsDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the mount follower outlived its context")
	}

	if detector.Dirty() {
		t.Fatal("stopping the follower was reported as coverage loss")
	}
}

// TestKernelMountBelowRoot mounts real tmpfs filesystems below a watched root and on a nested detector's root.
func TestKernelMountBelowRoot(t *testing.T) {
	t.Parallel()

	if os.Getenv("FSLEDGER_KERNEL_MOUNT") != "1" {
		t.Skip("opt in with FSLEDGER_KERNEL_MOUNT=1; mounts a tmpfs, as root or under unshare -rm")
	}

	root := t.TempDir()
	mounted, nested := filepath.Join(root, "mounted"), filepath.Join(root, "nested")

	for _, directory := range []string{mounted, nested} {
		err := os.Mkdir(directory, 0o700)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Detectors for a root and the nested mount found at startup, as the daemon starts them.
	unmountNested := mountTmpfs(t, nested)
	nestedDetector := startedWatcher(t.Context(), t, nested)
	detector := startedWatcher(t.Context(), t, root, nested)

	unmount := mountTmpfs(t, mounted)
	waitRemount(t, detector, mounted)

	if !detector.PollingRequired() {
		t.Fatal("the mounted tmpfs was left to no reconciliation")
	}

	writeFile(t, filepath.Join(mounted, "hidden"))
	writeFile(t, filepath.Join(root, "visible"))
	waitEvent(t, detector, filepath.Join(root, "visible"), filepath.Join(mounted, "hidden"))

	unmount()
	waitRemount(t, detector, mounted)

	if detector.PollingRequired() {
		t.Fatal("reconciliation outlived the unmounted tmpfs")
	}

	writeFile(t, filepath.Join(mounted, "uncovered"))
	waitEvent(t, detector, filepath.Join(mounted, "uncovered"), "")

	unmountNested()
	waitRemount(t, nestedDetector, nested)

	writeFile(t, filepath.Join(nested, "uncovered"))
	waitEvent(t, nestedDetector, filepath.Join(nested, "uncovered"), "")

	if nestedDetector.Dirty() || detector.Dirty() {
		t.Fatal("a mount table change was reported as coverage loss")
	}
}

// mountAt describes the filesystem with the given mount ID and device minor number mounted at point.
func mountAt(point string, id, minor int) mountinfo.Mount {
	return mountinfo.Mount{Point: point, Type: "tmpfs", Device: "0:" + strconv.Itoa(minor), ID: id}
}

func treeWatcher(t *testing.T, root string) *Watcher {
	t.Helper()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector, err := New(root, matcher)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.Close(detector) })

	err = detector.addTree(root)
	if err != nil {
		t.Fatal(err)
	}

	return detector
}

func startedWatcher(ctx context.Context, t *testing.T, root string, nested ...string) *Watcher {
	t.Helper()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector, err := New(root, matcher.Protect(nested...))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.Close(detector) })

	err = detector.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}

	return detector
}

// expectRemount takes the reconciliation request remount queued for point.
func expectRemount(t *testing.T, detector *Watcher, point string) {
	t.Helper()

	select {
	case raw := <-detector.Events():
		if raw.Dirty || raw.Path != point || raw.Reason != RemountReason || raw.Operation != event.Create {
			t.Fatal("wrong remount request", raw)
		}
	default:
		t.Fatal("no reconciliation requested for", point)
	}
}

func waitRemount(t *testing.T, detector *Watcher, point string) {
	t.Helper()

	deadline := time.After(3 * time.Second)

	for {
		select {
		case raw := <-detector.Events():
			if raw.Path == point && raw.Reason == RemountReason {
				return
			}
		case <-deadline:
			t.Fatal("no reconciliation requested for", point)
		}
	}
}

// waitEvent waits for an event on path, failing on an earlier one on unexpected.
func waitEvent(t *testing.T, detector *Watcher, path, unexpected string) {
	t.Helper()

	deadline := time.After(3 * time.Second)

	for {
		select {
		case raw := <-detector.Events():
			if raw.Path == unexpected {
				t.Fatal("a watch crossed into the mounted filesystem", raw)
			}

			if raw.Path == path {
				return
			}
		case <-deadline:
			t.Fatal("no event for", path)
		}
	}
}

func writeFile(t *testing.T, path string) {
	t.Helper()

	err := os.WriteFile(path, []byte("content"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

// mountTmpfs mounts a tmpfs at path until the returned function or the cleanup unmounts it.
func mountTmpfs(t *testing.T, path string) func() {
	t.Helper()

	err := unix.Mount("fsledger-test", path, "tmpfs", 0, "")
	if err != nil {
		t.Skipf("cannot mount tmpfs: %v", err)
	}

	unmount := func() {
		// EINVAL: the test already unmounted it.
		unmountErr := unix.Unmount(path, 0)
		if unmountErr != nil && !errors.Is(unmountErr, unix.EINVAL) {
			t.Error(unmountErr)
		}
	}
	t.Cleanup(unmount)

	return unmount
}
