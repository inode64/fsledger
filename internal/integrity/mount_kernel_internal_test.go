package integrity

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/exclude"
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

// CheckMounts reaches each recorded point before it reads the mount table: a mounted point passes, and its
// empty mount point does not.
func TestKernelCheckMountsReachesTheMountedPoint(t *testing.T) {
	t.Parallel()

	point := filepath.Join(t.TempDir(), "data")
	mountAt(t, "fsledger-test", point, "tmpfs", 0)

	err := CheckMounts([]string{point})
	if err != nil {
		t.Fatal("a mounted point was refused", err)
	}

	unmount(t, point)

	err = CheckMounts([]string{point})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatal("the empty mount point was accepted", err)
	}
}

// A filesystem unmounted while a scan walks it and mounted again before the scan ends left part of its tree
// unseen: the scan must fail, or the catalog prunes those entries as deleted. Without the unique mount ID the
// kernel may give the remounted filesystem the ID it had, so both cases are checked.
func TestKernelRemountDuringScanFailsTheScan(t *testing.T) {
	t.Parallel()

	for name, fill := range map[string]bool{"reused mount ID": false, "new mount ID": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			remountDuringScan(t, fill)
		})
	}
}

// remountDuringScan scans a bind-mounted root with 50 files, unmounting it at its first file and mounting
// it again at the file of a second root, before the scan's final root check.
func remountDuringScan(t *testing.T, fill bool) {
	t.Helper()

	data, base := t.TempDir(), t.TempDir()
	mounted, other := filepath.Join(base, "mounted"), filepath.Join(base, "other")

	for index := range 50 {
		writeKernelFile(t, filepath.Join(data, fmt.Sprintf("file%02d", index)))
	}

	writeKernelFile(t, filepath.Join(other, "file"))
	mountAt(t, data, mounted, "", unix.MS_BIND)

	var (
		unmounted, remounted sync.Once
		mountErr             error
	)

	// Each regular file is hashed in a worker, so the hook runs while the walk goes on.
	hook := func(record Record, _ string) (string, int64, bool) {
		path := string(record.Path)
		if strings.HasPrefix(path, mounted+"/") {
			unmounted.Do(func() { mountErr = unix.Unmount(mounted, unix.MNT_DETACH) })
		}

		if strings.HasPrefix(path, other+"/") {
			remounted.Do(func() {
				if fill {
					// Another mount takes the released mount ID, as any mount elsewhere meanwhile would.
					mountAt(t, "fsledger-test", filepath.Join(base, "filler"), "tmpfs", 0)
				}

				mountErr = errors.Join(mountErr, unix.Mount(data, mounted, "", unix.MS_BIND, ""))
			})
		}

		return "", 0, false
	}

	seen, err := scanCounting(t, []string{mounted, other}, mounted, hook)
	if mountErr != nil {
		t.Fatal(mountErr)
	}

	if seen < 50 && !errors.Is(err, ErrUnavailable) {
		t.Fatalf("scan accepted although %d of 50 files went unseen while unmounted: %v", 50-seen, err)
	}
}

// scanCounting scans roots and counts the records observed below prefix.
func scanCounting(
	t *testing.T, roots []string, prefix string, hook func(Record, string) (string, int64, bool),
) (int, error) {
	t.Helper()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	seen := 0
	err = NewScanner(1, 1).ScanPaths(t.Context(), roots, nil, matcher, "sha256", true, true,
		func(record Record) error {
			if strings.HasPrefix(string(record.Path), prefix+"/") {
				seen++
			}

			return nil
		}, hook)

	return seen, err
}

func writeKernelFile(t *testing.T, path string) {
	t.Helper()

	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		err = os.WriteFile(path, []byte("x"), 0o600)
	}

	if err != nil {
		t.Fatal(err)
	}
}
