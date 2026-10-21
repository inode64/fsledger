package git

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

// Loose objects fan out by the first two hex digits of their identifier.
const fanoutLength = 2

// claim takes over a verified mirror whose directory lock the caller holds. No other Git
// process writes there: the daemon's repository worker and the inventory commands run Git
// serially under that lock, while restores only read, without locks (--no-optional-locks).
// Any existing lock is therefore orphaned, and later cancellations may release their own.
func (r *Repository) claim() error {
	r.exclusive = true

	err := r.removeStaleLocks()
	if err != nil {
		return err
	}

	return r.preserveContent()
}

// removeStaleLocks deletes the lock files of Git commands that were killed or outlived a
// crashed daemon. Git never expires them, so every later write would fail with an opaque
// exit status. Only an exclusive owner may call it; descendants that escaped a killed
// process group are not covered.
func (r *Repository) removeStaleLocks() error {
	metadata, err := os.OpenRoot(filepath.Join(r.Path, ".git"))
	if err != nil {
		return fault.Wrap("open Git metadata", err)
	}
	defer resource.Close(metadata)

	locks := make([]string, 0)

	err = fs.WalkDir(metadata.FS(), ".", func(name string, entry fs.DirEntry, visitErr error) error {
		switch {
		case errors.Is(visitErr, fs.ErrNotExist):
			// A killed process may still complete its last rename or removal.
			return nil
		case visitErr != nil:
			return visitErr
		case entry.IsDir() && looseObjects(name):
			return fs.SkipDir
		case entry.Type().IsRegular() && strings.HasSuffix(name, ".lock"):
			locks = append(locks, name)
		}

		return nil
	})
	if err != nil {
		return fault.Wrap("find stale Git locks", err)
	}

	for _, name := range locks {
		err = metadata.Remove(name)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fault.Wrap("remove stale Git lock", err)
		}
	}

	return nil
}

// Loose object fan-out directories hold no locks but most of a large mirror's files.
func looseObjects(name string) bool {
	return path.Dir(name) == "objects" && len(path.Base(name)) == fanoutLength
}
