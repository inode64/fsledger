package fanotify

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/resource"
	"github.com/inode64/fsledger/internal/watcher"
)

// A filesystem mounted below the root after Start used to be marked by the next walk. Its events carry another
// fsid, which the root's handle anchor cannot resolve: every refresh flushed the marks, marked it again and lost
// coverage again. A filesystem without FID support failed the walk for good. A rootless test cannot mount one, so
// the directories it does not remember stand for it: the root's filesystem is recorded as another one.
func TestFilesystemMountedBelowRootIsABoundary(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	plain, mounted := filepath.Join(root, "plain"), filepath.Join(root, "mounted")

	for _, directory := range []string{plain, filepath.Join(mounted, "inner")} {
		err := os.MkdirAll(directory, 0o700)
		if err != nil {
			t.Fatal(err)
		}
	}

	detector := rememberedTree(t, root, plain)
	detector.device++ // Only the remembered directories keep passing for the root's filesystem.
	detector.refreshCoverage()

	if detector.Dirty() || detector.coverageLost.Load() {
		t.Fatal("a filesystem mounted below the root was reported as lost coverage")
	}

	if _, marked := detector.marks.Load(mounted); marked || detector.Covers(filepath.Join(mounted, "file")) {
		t.Fatal("marks crossed into another filesystem")
	}

	if !detector.Covers(filepath.Join(plain, "file")) || !detector.PollingRequired() {
		t.Fatal("the unmarked filesystem was left to neither marks nor reconciliation")
	}

	// A walk of a subtree cannot tell that the other filesystem is gone.
	err := detector.markTree(plain, nil)
	if err != nil || !detector.PollingRequired() {
		t.Fatal("a subtree walk forgot the unmarked filesystem", err)
	}

	err = os.RemoveAll(mounted)
	if err != nil {
		t.Fatal(err)
	}

	detector.refreshCoverage()

	if detector.PollingRequired() || detector.Dirty() {
		t.Fatal("reconciliation outlived the unmarked filesystem")
	}
}

// Another filesystem at the root itself is no boundary: the handle anchor and the marks belong to the one it hid.
func TestFilesystemReplacingRootLosesCoverage(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	detector := rememberedTree(t, root)
	detector.marks.Clear()
	detector.device++

	err := detector.markTree(root, nil)
	if !errors.Is(err, errOtherFilesystem) {
		t.Fatal("the root's own filesystem was skipped like a boundary", err)
	}
}

// TestKernelMountBelowRoot mounts a real tmpfs below a marked root. Marks and FID records need no privileges;
// the handle anchor is left out because resolving handles needs CAP_DAC_READ_SEARCH.
func TestKernelMountBelowRoot(t *testing.T) {
	t.Parallel()

	if os.Getenv("FSLEDGER_KERNEL_MOUNT") != "1" {
		t.Skip("opt in with FSLEDGER_KERNEL_MOUNT=1; mounts a tmpfs, as root or under unshare -rm")
	}

	root := t.TempDir()
	mounted := filepath.Join(root, "mounted")

	err := os.Mkdir(mounted, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	detector := markedGroup(t, root)
	defer resource.Close(detector)

	unmount := mountTmpfs(t, mounted)

	err = os.Mkdir(filepath.Join(mounted, "inner"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	detector.refreshCoverage()

	// Without an anchor, any event the group delivers is a loss, so a single write proves the tmpfs unmarked.
	err = os.WriteFile(filepath.Join(mounted, "inner", "file"), []byte("mounted after Start"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = detector.readBatch(make([]byte, eventBufferSize))
	if err != nil || len(detector.queue.Channel) != 0 || detector.Dirty() || !detector.PollingRequired() {
		t.Fatal("marks crossed into the mounted tmpfs or left it to no reconciliation", err)
	}

	unmount()
	detector.refreshCoverage()

	if detector.PollingRequired() || detector.Dirty() {
		t.Fatal("reconciliation outlived the unmounted tmpfs")
	}
}

// markedGroup marks root in an unprivileged FID group, as Start does.
func markedGroup(t *testing.T, root string) *Watcher {
	t.Helper()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector := new(Watcher)
	detector.fd = -1
	detector.path = root
	detector.matcher = matcher
	detector.queue = watcher.NewQueue(watcher.QueueSize)
	detector.refreshPaths = make(chan string, 1)
	detector.refreshAll = make(chan struct{}, 1)

	err = detector.tryMode(unix.FAN_REPORT_FID|unix.FAN_REPORT_DFID_NAME, 0)
	if err != nil {
		t.Skipf("fanotify FID groups unavailable: %v", err)
	}

	err = detector.markTree(root, nil)
	if err != nil {
		resource.Close(detector)
		t.Fatal(err)
	}

	return detector
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
