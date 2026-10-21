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
		// Validation keeps storage and runtime apart, so only the runtime path equals it.
		role := "storage"
		if path == c.Runtime {
			role = "runtime"
		}

		err := checkDirectory(role, path)
		if err != nil {
			return err
		}
	}

	return nil
}

// checkDirectory requires the nearest existing component of path to be a real directory without
// symlinks. Log, storage and runtime share it, so role names which configured location failed.
func checkDirectory(role, path string) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		subject := role + " " + path
		if current != path {
			subject += " parent " + current
		}

		if err != nil {
			return fault.Wrap("inspect "+subject, err)
		}

		if info.Mode()&os.ModeSymlink != 0 {
			return fault.New(subject + " is a symlink")
		}

		if !info.IsDir() {
			return fault.New(subject + " is not a directory")
		}

		resolved, err := filepath.EvalSymlinks(current)
		if err != nil {
			return fault.Wrap("resolve "+subject, err)
		}

		if resolved != current {
			return fault.New(subject + " resolves through a symlink to " + resolved)
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
