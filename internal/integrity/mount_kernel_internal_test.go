package integrity

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// mountTmpfs mounts a private tmpfs on a new directory at path, for tests that opted in, and unmounts it
// at cleanup unless the test already did.
func mountTmpfs(t *testing.T, path string) {
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

func unmount(t *testing.T, path string) {
	t.Helper()

	err := unix.Unmount(path, 0)
	if err != nil {
		t.Fatal(err)
	}
}

// A filesystem unmounted from a root is lost, and one mounted again at the same point is the same source,
// although the kernel gives it a new mount ID: the root must not stay unavailable until the daemon exits.
func TestKernelRemountedRootIsTheSameSource(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "data")
	mountTmpfs(t, root)

	guard := RootGuard{}

	err := guard.Check([]string{root})
	if err != nil {
		t.Fatal(err)
	}

	unmount(t, root)

	err = guard.Check([]string{root})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatal("the empty mount point was taken for the source", err)
	}

	// Another mount takes the identifiers the first one released, so the remount gets new ones.
	mountTmpfs(t, filepath.Join(t.TempDir(), "other"))
	mountTmpfs(t, root)

	err = guard.Check([]string{root})
	if err != nil {
		t.Fatal("the filesystem mounted again at the root stayed unavailable", err)
	}

	// A filesystem stacked over the pinned one at the same point hides the recorded tree.
	mountTmpfs(t, root)

	err = guard.Check([]string{root})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatal("a filesystem stacked over the source was taken for it", err)
	}
}
