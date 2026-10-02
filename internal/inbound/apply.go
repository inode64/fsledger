package inbound

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/pathutil"
	"github.com/inode64/fsledger/internal/resource"
)

// Apply is idempotent across a crash between replacement and the progress save.
// The caller must durably save the plan before calling it, and serialize each repository.
func (plan *Plan) Apply(ctx context.Context, objects Objects, save func() error) error {
	for index, change := range plan.Changes {
		err := ctx.Err()
		if err != nil {
			return fault.Wrap("incoming application cancelled", err)
		}

		err = plan.CheckRoots()
		if err != nil {
			return err
		}

		err = plan.applyOne(ctx, objects, index, change)
		if err != nil {
			return pathError(err)
		}

		if index < plan.Done {
			continue
		}

		previous := plan.Done
		plan.Done = index + 1

		err = save()
		if err != nil {
			plan.Done = previous

			return err
		}
	}

	return nil
}

func (plan *Plan) applyOne(ctx context.Context, objects Objects, index int, change Change) error {
	parent, name, err := plan.openParent(string(change.Path), change.After.Mode != absent)
	if err != nil {
		return err
	}
	defer resource.FD(parent)

	err = clearTemporary(parent, tempName(plan, index))
	if err != nil {
		return err
	}

	actual, err := readAt(parent, name)
	if err != nil {
		return err
	}

	data, err := targetBytes(ctx, objects, change.After)
	if err != nil {
		return err
	}

	if actual.mode == change.After.Mode && bytes.Equal(actual.data, data) {
		return nil
	}

	if index < plan.Done {
		return conflict("previously applied source changed: " + string(change.Path))
	}

	equal, err := matches(ctx, objects, actual, change.Before)
	if err != nil {
		return err
	}

	if !equal {
		return conflict("source changed before incoming replacement: " + string(change.Path))
	}

	return plan.replace(parent, name, tempName(plan, index), change, actual, data)
}

func (plan *Plan) replace(parent int, name, temp string, change Change, actual observed, data []byte) error {
	// A durable unique name lets recovery remove a partially written temporary.
	err := unix.Unlinkat(parent, temp, 0)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fault.Wrap("remove incoming temporary", err)
	}

	if change.After.Mode != absent {
		err = writeTemporary(parent, name, temp, change.After.Mode, actual, data)
		if err != nil {
			removeTemporary(parent, temp)

			return err
		}
		defer removeTemporary(parent, temp)
	}

	err = plan.CheckRoots()
	if err == nil {
		err = verifyDestination(parent, name, string(change.Path), actual)
	}

	if err != nil {
		return err
	}

	switch {
	case change.After.Mode == absent:
		err = unix.Unlinkat(parent, name, 0)
	case actual.mode == absent:
		err = unix.Renameat2(parent, temp, parent, name, unix.RENAME_NOREPLACE)
	default:
		err = unix.Renameat(parent, temp, parent, name)
	}

	if err != nil {
		return fault.Wrap("replace incoming destination", err)
	}

	return fault.Wrap("sync incoming parent", unix.Fsync(parent))
}

func verifyDestination(parent int, name, path string, actual observed) error {
	currentParent, _, err := integrity.OpenParent(path)
	if err != nil {
		return err
	}
	defer resource.FD(currentParent)

	var pinned, current unix.Stat_t
	if unix.Fstat(parent, &pinned) != nil || unix.Fstat(currentParent, &current) != nil ||
		pinned.Dev != current.Dev || pinned.Ino != current.Ino {
		return conflict("incoming parent moved: " + path)
	}

	err = unix.Fstatat(parent, name, &current, unix.AT_SYMLINK_NOFOLLOW)
	if actual.mode == absent && errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil || actual.mode == absent || !sameStat(actual.stat, current) {
		return conflict("incoming destination changed before replacement: " + path)
	}

	return nil
}

func (plan *Plan) openParent(path string, create bool) (int, string, error) {
	for _, root := range plan.Roots {
		if !pathutil.Contains(string(root.Path), path) {
			continue
		}

		return openAnchoredParent(root, path, create)
	}

	return -1, "", conflict("incoming destination has no pinned root")
}

func openAnchoredParent(root Root, path string, create bool) (int, string, error) {
	parent, name, err := integrity.OpenParent(string(root.Anchor))
	if err != nil {
		return -1, "", err
	}

	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	resource.FD(parent)

	if err != nil {
		return -1, "", fault.Wrap("open incoming root", err)
	}

	relative, err := filepath.Rel(string(root.Anchor), filepath.Dir(path))
	if err != nil || !filepath.IsLocal(relative) {
		resource.FD(fd)

		return -1, "", conflict("incoming path escapes pinned root")
	}

	if relative != "." {
		fd, err = descend(fd, strings.Split(relative, "/"), create)
	}

	return fd, filepath.Base(path), err
}

func descend(fd int, parts []string, create bool) (int, error) {
	for _, part := range parts {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, os.ErrNotExist) && create {
			err = unix.Mkdirat(fd, part, 0o700)
			if err == nil {
				err = unix.Fsync(fd)
			}

			if err == nil || errors.Is(err, os.ErrExist) {
				next, err = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			}
		}

		resource.FD(fd)

		if err != nil {
			return -1, fault.Wrap("open incoming parent", err)
		}

		fd = next
	}

	return fd, nil
}

func clearTemporary(parent int, name string) error {
	err := unix.Unlinkat(parent, name, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fault.Wrap("remove incoming temporary", err)
	}

	return fault.Wrap("sync incoming temporary removal", unix.Fsync(parent))
}

func removeTemporary(parent int, name string) {
	// Cleanup is retried using the persisted unique name after an interrupted application.
	err := clearTemporary(parent, name)
	if err != nil {
		slog.Warn("incoming temporary cleanup failed", "error", err)
	}
}
