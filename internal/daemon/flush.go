package daemon

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

const flushRequestSuffix = ".flush"

// RequestFlush queues an explicit request for the running owner; it never opens its database.
func RequestFlush(cfg *config.Config, name string) error {
	repo, exists := cfg.Repositories[name]
	if !exists || repo.Type != config.RepositoryGit {
		return fault.New("flush requires an existing Git repository")
	}

	err := cfg.CheckStoragePaths()
	if err != nil {
		return err
	}

	root, err := os.OpenRoot(cfg.Runtime)
	if err != nil {
		return fault.Wrap("open runtime for flush request", err)
	}
	defer resource.Close(root)
	// Exclusive creation refuses links and lets repeated requests remain idempotent.
	file, err := root.OpenFile(name+flushRequestSuffix, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		info, statErr := root.Lstat(name + flushRequestSuffix)
		if statErr != nil {
			return fault.Wrap("inspect flush request", statErr)
		}

		if !info.Mode().IsRegular() {
			return fault.New("flush request must be a regular file")
		}

		return nil
	}

	if err != nil {
		return fault.Wrap("write flush request", err)
	}

	return fault.Wrap("close flush request", file.Close())
}

func (w *worker) checkFlushRequest() error {
	if w.repo == nil || w.forceCommit {
		return nil
	}

	info, err := os.Lstat(filepath.Join(w.cfg.Runtime, w.name+flushRequestSuffix))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fault.Wrap("inspect flush request", err)
	}

	if !info.Mode().IsRegular() {
		return fault.New("flush request must be a regular file")
	}

	w.forceCommit = true
	w.dirty = true
	// A flush is also the administrator's way to adopt new sources without waiting.
	w.refreshSources()

	return nil
}

func (w *worker) completeFlushRequest() error {
	if !w.forceCommit || len(w.unstable) > 0 {
		return nil
	}

	err := os.Remove(filepath.Join(w.cfg.Runtime, w.name+flushRequestSuffix))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fault.Wrap("remove flush request", err)
	}

	w.forceCommit = false

	return nil
}
