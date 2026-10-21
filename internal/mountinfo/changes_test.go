package mountinfo_test

import (
	"errors"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/mountinfo"
	"github.com/inode64/fsledger/internal/resource"
)

type waitResult struct {
	err     error
	changed bool
}

// A watcher closing must not wait for the next mount: Stop ends the pending Wait and every later one.
func TestStopEndsWait(t *testing.T) {
	t.Parallel()

	changes, err := mountinfo.FollowChanges()
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(changes)

	result := waitChange(changes)

	changes.Stop()
	changes.Stop()

	for _, waited := range []<-chan waitResult{result, waitChange(changes)} {
		select {
		case got := <-waited:
			if got.changed || got.err != nil {
				t.Fatal("a stopped wait reported a change", got)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Stop did not end the wait")
		}
	}
}

// TestKernelMountWakesWait mounts and unmounts a real tmpfs while waiting.
func TestKernelMountWakesWait(t *testing.T) {
	t.Parallel()

	if os.Getenv("FSLEDGER_KERNEL_MOUNT") != "1" {
		t.Skip("opt in with FSLEDGER_KERNEL_MOUNT=1; mounts a tmpfs, as root or under unshare -rm")
	}

	changes, err := mountinfo.FollowChanges()
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(changes)

	path := t.TempDir()

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

	expectChange(t, waitChange(changes))

	err = unix.Unmount(path, 0)
	if err != nil {
		t.Fatal(err)
	}

	expectChange(t, waitChange(changes))
}

func waitChange(changes *mountinfo.Changes) <-chan waitResult {
	result := make(chan waitResult, 1)

	go func() {
		changed, err := changes.Wait()
		result <- waitResult{err: err, changed: changed}
	}()

	return result
}

func expectChange(t *testing.T, result <-chan waitResult) {
	t.Helper()

	select {
	case got := <-result:
		if !got.changed || got.err != nil {
			t.Fatal("the mount table change was not reported", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the mount table change did not end the wait")
	}
}
