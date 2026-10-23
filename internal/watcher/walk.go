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
// Directories whose path reaches PATH_MAX are not walked, and the result reports them: every level holds a
// descriptor, and such paths cannot be named in events anyway. Reconciliation still scans them.
func WalkDirectories(root string, skip func(string) bool, visit func(path string, directory int) error) (bool, error) {
	if len(root) >= unix.PathMax {
		return true, nil
	}

	parent, name, err := integrity.OpenParent(root)
	if integrity.Missing(err) {
		return false, nil
	}

	if err != nil {
		return false, fault.Wrap("open watched root parent", err)
	}

	defer resource.FD(parent)

	walk := directoryWalk{skip: skip, visit: visit, deep: false}
	err = walk.at(parent, name, root)

	return walk.deep, err
}

type directoryWalk struct {
	skip  func(string) bool
	visit func(string, int) error
	deep  bool
}

//nolint:gocognit // One level of the walk: bound, prune, open, visit, list and descend stay together.
func (walk *directoryWalk) at(parent int, name, path string) error {
	if len(path) >= unix.PathMax {
		walk.deep = true

		return nil
	}

	if walk.skip(path) {
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

	err = walk.visit(path, fd)
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
			err = walk.at(fd, entry.Name(), filepath.Join(path, entry.Name()))
			if err != nil {
				return err
			}
		}
	}

	return nil
}
