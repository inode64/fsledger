package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/inode64/fsledger/internal/pathutil"

	"github.com/inode64/fsledger/internal/fault"
)

// CleanAbsolute rejects traversal instead of silently normalizing it.
func CleanAbsolute(path string) error {
	if !pathutil.ValidAbsolute(path) {
		return fault.New(fmt.Sprintf("path must be clean and absolute: %q", path))
	}

	return nil
}

// Overlaps reports whether either absolute path contains the other.
func Overlaps(first, second string) bool {
	return pathutil.Contains(first, second) || pathutil.Contains(second, first)
}

// CheckPaths reads filesystem information but never creates files. Sources may be absent:
// ResolveSources reports what currently exists.
func (c *Config) CheckPaths() error {
	if c.Logging.File != "" {
		logPathErr := CheckLogPath(c.Logging.File)
		if logPathErr != nil {
			return logPathErr
		}
	}

	return c.CheckStoragePaths()
}

// CheckStoragePaths validates state directories without requiring source paths to exist.
// Importing an offline reference must not depend on the current source filesystem.
func (c *Config) CheckStoragePaths() error {
	for _, path := range c.InternalPaths() {
		err := checkStorage(path)
		if err != nil {
			return err
		}
	}

	return nil
}

func checkStorage(path string) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return fmt.Errorf("storage/runtime %s: %w", current, err)
		}

		if !info.IsDir() {
			return fault.New("storage/runtime parent is not a directory: " + current)
		}

		resolved, err := filepath.EvalSymlinks(current)
		if err != nil {
			return fault.Wrap("resolve storage", err)
		}

		if resolved != current {
			return fault.New("storage/runtime contains symlink: " + current)
		}

		return nil
	}
}

// CheckRepositoryStorage rejects the other backend's layout before opening state.
func CheckRepositoryStorage(root, kind string) error {
	reserved := ".git"
	if kind == RepositoryGit {
		reserved = CatalogDirectory
	}

	_, err := os.Lstat(filepath.Join(root, reserved))
	if err == nil {
		return fault.New("repository storage belongs to another backend")
	}

	if !errors.Is(err, os.ErrNotExist) {
		return fault.Wrap("inspect repository storage", err)
	}

	return nil
}
