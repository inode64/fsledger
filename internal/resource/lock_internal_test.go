package resource

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestDirectoryLockReleasesInheritedDescriptor(t *testing.T) {
	t.Parallel()
	path := t.TempDir()

	lock, err := LockDirectory(path)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if lock != nil {
			Close(lock)
		}
	})

	// A duplicate holds the same open file description as a child between fork
	// and exec, without depending on subprocess scheduling to reproduce the bug.
	inherited, err := unix.FcntlInt(lock.file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { FD(inherited) })

	other, err := LockDirectory(path)
	if err == nil {
		Close(other)
		t.Fatal("repository admitted a concurrent owner")
	}

	err = lock.Close()
	lock = nil

	if err != nil {
		t.Fatal(err)
	}

	other, err = LockDirectory(path)
	if err != nil {
		t.Fatal("closed owner left its repository locked by an inherited descriptor:", err)
	}

	Close(other)
}
