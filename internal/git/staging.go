package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/pathutil"
	"github.com/inode64/fsledger/internal/resource"

	"github.com/inode64/fsledger/internal/fault"
)

// CommitPaths stages affected relative paths, including subtrees and removals.
// The worker must use Commit for full reconciliation after any partial failure.
func (r *Repository) CommitPaths(ctx context.Context, message string, paths []string) (string, error) {
	if len(paths) == 0 {
		return "", nil
	}

	err := r.stage(ctx, paths)
	if err != nil {
		return "", err
	}

	return r.CommitStaged(ctx, message)
}

func (r *Repository) stage(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}

	present, missing, err := r.stagePaths(paths)
	if err != nil {
		return err
	}

	if present != "" {
		_, err = r.runInput(ctx, present, "add", "--all", "--force", "--pathspec-from-file=-", "--pathspec-file-nul")
		if err != nil {
			return err
		}
	}

	if missing != "" {
		// An event can refer to a transient file that never reached the index.
		_, err = r.runInput(ctx, missing, "rm", "--cached", "--force", "--ignore-unmatch", "-r",
			"--pathspec-from-file=-", "--pathspec-file-nul")
		if err != nil {
			return err
		}
	}

	return nil
}

func (r *Repository) stagePaths(paths []string) (string, string, error) {
	root, err := unix.Open(r.Path, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", "", fault.Wrap("open staging root", err)
	}
	defer resource.FD(root)

	present := make([]byte, 0)
	missing := make([]byte, 0)

	for _, path := range paths {
		if !pathutil.ValidRelative(path) {
			return "", "", fault.New("staging path must be relative to the mirror")
		}

		err := inspectStagingPath(root, path)
		switch {
		case err == nil:
			present = append(present, path...)
			present = append(present, 0)
		case errors.Is(err, os.ErrNotExist), errors.Is(err, unix.ENOTDIR):
			missing = append(missing, path...)
			missing = append(missing, 0)
		default:
			return "", "", fault.Wrap("inspect staging path", err)
		}
	}

	return string(present), string(missing), nil
}

// Resolve parents by descriptor, rejecting symlinks even when they point inside the worktree.
func inspectStagingPath(root int, path string) error {
	parts := strings.Split(path, string(filepath.Separator))

	parent := root
	defer func() {
		if parent != root {
			resource.FD(parent)
		}
	}()

	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(parent, part, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return stagingParentError(parent, part, err)
		}

		if parent != root {
			resource.FD(parent)
		}

		parent = next
	}

	var stat unix.Stat_t

	return fault.Wrap("stat staging entry", unix.Fstatat(parent, parts[len(parts)-1], &stat, unix.AT_SYMLINK_NOFOLLOW))
}

func stagingParentError(parent int, name string, openErr error) error {
	if !errors.Is(openErr, unix.ENOTDIR) {
		return fault.Wrap("open staging parent", openErr)
	}
	// O_DIRECTORY|O_NOFOLLOW reports ENOTDIR for links too. Only a real non-directory
	// is a missing descendant (for example after replacing a tracked directory with a file).
	var stat unix.Stat_t

	err := unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return fault.Wrap("inspect staging parent", err)
	}

	if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
		return fault.New("staging parent changed during resolution")
	}

	if stat.Mode&unix.S_IFMT == unix.S_IFLNK {
		return fault.New("staging parent must not be a symlink")
	}

	return fault.Wrap("open staging parent", openErr)
}
