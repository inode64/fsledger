package integrity

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// mountAt mounts source on target, a directory it creates, for tests that opted in, and unmounts it at
// cleanup unless the test already did. A bind mount passes fstype "" and unix.MS_BIND.
func mountAt(t *testing.T, source, target, fstype string, flags uintptr) {
	t.Helper()

	if os.Getenv("FSLEDGER_KERNEL_MOUNT") != "1" {
		t.Skip("opt in with FSLEDGER_KERNEL_MOUNT=1; mounts a tmpfs, as root or under unshare -rm")
	}

	err := os.MkdirAll(target, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = unix.Mount(source, target, fstype, flags, "")
	if err != nil {
		t.Skipf("cannot mount %s: %v", source, err)
	}

	t.Cleanup(func() {
		// EINVAL: the test already unmounted it.
		unmountErr := unix.Unmount(target, 0)
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

// remountedGuard pins a root bound from data, unmounts it and checks that the empty mount point is rejected.
func remountedGuard(t *testing.T, data, root string) *RootGuard {
	t.Helper()

	mountAt(t, data, root, "", unix.MS_BIND)

	guard := &RootGuard{}

	err := guard.Check([]string{root})
	if err != nil {
		t.Fatal(err)
	}

	unmount(t, root)

	err = guard.Check([]string{root})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatal("the empty mount point was taken for the source", err)
	}

	// Another mount takes the identifiers the first one released, so the next one gets new ones.
	mountAt(t, "fsledger-test", filepath.Join(t.TempDir(), "other"), "tmpfs", 0)

	return guard
}

// The filesystem mounted again at the root's mount point is the same source although the kernel gives it a
// new mount ID: the root must not stay unavailable until the daemon exits.
func TestKernelRemountedRootIsTheSameSource(t *testing.T) {
	t.Parallel()

	data := t.TempDir()
	root := filepath.Join(t.TempDir(), "data")
	guard := remountedGuard(t, data, root)

	mountAt(t, data, root, "", unix.MS_BIND)

	err := guard.Check([]string{root})
	if err != nil {
		t.Fatal("the filesystem mounted again at the root stayed unavailable", err)
	}
}
