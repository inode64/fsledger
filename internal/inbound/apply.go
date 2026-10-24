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
	// A conflict at an earlier index would otherwise stop before reaching the interrupted replacement.
	err := plan.ClearTemporary()
	if err != nil {
		return err
	}

	for index, change := range plan.Changes {
		err = ctx.Err()
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

// ClearTemporary removes remote content left by an interrupted replacement of the next pending change;
// callers abandoning the plan need it because only a later application would reach that change.
func (plan *Plan) ClearTemporary() error {
	if plan.Done < 0 || plan.Done >= len(plan.Changes) {
		return nil
	}

	err := plan.CheckRoots()
	if err != nil {
		return err
	}

	parent, _, err := plan.openParent(string(plan.Changes[plan.Done].Path), false)
	// Only a creation adds missing parents, before its temporary.
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return pathError(err)
	}

	defer resource.FD(parent)

	return clearTemporary(parent, tempName(plan, plan.Done))
}

func (plan *Plan) applyOne(ctx context.Context, objects Objects, index int, change Change) error {
	// Only a pending creation may add parents: any other path with a missing parent changed locally.
	create := index >= plan.Done && change.Before.Mode == absent && change.After.Mode != absent

	parent, name, err := plan.openParent(string(change.Path), create)
	if errors.Is(err, os.ErrNotExist) {
		if change.After.Mode == absent {
			return nil
		}

		return conflict("incoming destination parent is missing: " + string(change.Path))
	}

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
		// A prior attempt may have renamed or unlinked the entry before its parent sync failed.
		// Make that replacement durable before the caller records progress, even when no write remains.
		return integrity.SyncDirectory(parent)
	}

	if index < plan.Done {
		return conflict("previously applied source changed: " + string(change.Path))
	}

	if !matches(actual, change.Before) {
		return conflict("source changed before incoming replacement: " + string(change.Path))
	}

	return plan.replace(parent, name, tempName(plan, index), change, actual, data)
}

// replace runs after applyOne cleared the temporary: a durable unique name lets recovery remove a partial one.
func (plan *Plan) replace(parent int, name, temp string, change Change, actual observed, data []byte) error {
	if change.After.Mode != absent {
		err := writeTemporary(parent, name, temp, change.After.Mode, actual, data)
		if err != nil {
			removeTemporary(parent, temp)

			return err
		}
		defer removeTemporary(parent, temp)
	}

	err := plan.CheckRoots()
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

	return fault.Wrap("sync incoming parent", integrity.SyncDirectory(parent))
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

	// Pinned like every parent below it, so all of them are synced through SyncDirectory.
	fd, err := unix.Openat(parent, name, integrity.DirectoryFlags, 0)
	resource.FD(parent)

	if err != nil {
		return -1, "", fault.Wrap("open incoming root", err)
	}

	// CheckRoots compared the path earlier; the descriptor itself must still be the pinned root.
	var info unix.Statx_t

	err = unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_INO|unix.STATX_MNT_ID, &info)
	if err != nil || unix.Mkdev(info.Dev_major, info.Dev_minor) != root.Device || info.Ino != root.Inode ||
		info.Mnt_id != root.Mount {
		resource.FD(fd)

		return -1, "", conflict("incoming destination identity changed: " + string(root.Path))
	}

	relative, err := filepath.Rel(string(root.Anchor), filepath.Dir(path))
	if err != nil || !filepath.IsLocal(relative) {
		resource.FD(fd)

		return -1, "", conflict("incoming path escapes pinned root")
	}

	mode := uint32(0)
	if create {
		mode = 0o700
	}

	if relative != "." {
		fd, err = integrity.Descend(fd, strings.Split(relative, "/"), mode)
	}

	return fd, filepath.Base(path), err
}

func clearTemporary(parent int, name string) error {
	err := unix.Unlinkat(parent, name, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fault.Wrap("remove incoming temporary", err)
	}

	return fault.Wrap("sync incoming temporary removal", integrity.SyncDirectory(parent))
}

func removeTemporary(parent int, name string) {
	// Cleanup is retried using the persisted unique name after an interrupted application.
	err := clearTemporary(parent, name)
	if err != nil {
		slog.Warn("incoming temporary cleanup failed", "error", err)
	}
}
