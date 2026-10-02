package resource

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
)

// DirectoryLock owns a repository independently of incidental inherited descriptors.
type DirectoryLock struct {
	file *os.File
}

// LockDirectory owns a repository until the returned lock is closed.
func LockDirectory(path string) (*DirectoryLock, error) {
	err := os.MkdirAll(path, 0o700)
	if err != nil {
		return nil, fault.Wrap("create repository", err)
	}
	//nolint:gosec // The caller supplies a validated repository storage directory.
	file, err := os.Open(path)
	if err != nil {
		return nil, fault.Wrap("open repository directory", err)
	}

	err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err != nil {
		Close(file)

		return nil, fault.New("repository is in use; stop its daemon before inventory commands")
	}

	return &DirectoryLock{file: file}, nil
}

// Close explicitly unlocks before closing: a concurrent fork can retain the open
// file description until exec, even when the descriptor has close-on-exec set.
func (lock *DirectoryLock) Close() error {
	err := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)

	return errors.Join(fault.Wrap("unlock repository", err), lock.file.Close())
}
