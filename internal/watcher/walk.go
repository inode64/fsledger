package watcher

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/resource"
)

// WalkDirectories visits root and the directories below it, opening each relative to its parent without
// following symlinks: a directory replaced by a symlink during the walk is never entered, nor is anything
// it points to. skip prunes a directory before it is opened. visit receives each opened directory and may
// return fs.SkipDir. A directory gone or replaced meanwhile is skipped: its parent reports the change.
func WalkDirectories(root string, skip func(string) bool, visit func(path string, directory int) error) error {
	parent, name, err := integrity.OpenParent(root)
	if integrity.Missing(err) {
		return nil
	}

	if err != nil {
		return fault.Wrap("open watched root parent", err)
	}

	defer resource.FD(parent)

	return walkAt(parent, name, root, skip, visit)
}

func walkAt(parent int, name, path string, skip func(string) bool, visit func(string, int) error) error {
	if skip(path) {
		return nil
	}

	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if integrity.Missing(err) || errors.Is(err, unix.ELOOP) {
		return nil
	}

	if err != nil {
		return fault.Wrap("open watched directory "+path, err)
	}

	directory := os.NewFile(uintptr(fd), path)
	defer resource.Close(directory)

	err = visit(path, fd)
	if errors.Is(err, fs.SkipDir) {
		return nil
	}

	if err != nil {
		return err
	}

	entries, err := directory.ReadDir(-1)
	if integrity.Missing(err) {
		return nil
	}

	if err != nil {
		return fault.Wrap("list watched directory "+path, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			err = walkAt(fd, entry.Name(), filepath.Join(path, entry.Name()), skip, visit)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
