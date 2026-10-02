package inotify

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/resource"
)

func TestSiblingCreateDoesNotInstallRecursiveWatches(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()

	root, sibling := filepath.Join(parent, "root"), filepath.Join(parent, "sibling")
	for _, path := range []string{root, filepath.Join(sibling, "nested")} {
		err := os.MkdirAll(path, 0o700)
		if err != nil {
			t.Fatal(err)
		}
	}

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector, err := New(root, matcher)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(detector)

	err = detector.native.Add(parent)
	if err != nil {
		t.Fatal(err)
	}

	detector.handle(fsnotify.Event{Name: sibling, Op: fsnotify.Create})

	if got := detector.native.WatchList(); len(got) != 1 {
		t.Fatal("installed sibling watches", got)
	}

	detector.handle(fsnotify.Event{Name: root, Op: fsnotify.Create})

	if got := detector.native.WatchList(); len(got) != 2 {
		t.Fatal("lost root recreation coverage", got)
	}
}

// A directory removed between the walk listing it and its watch installation is not coverage loss.
func TestVanishedDirectoryIsNotCoverageLoss(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()

	file := filepath.Join(parent, "file")

	err := os.WriteFile(file, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector, err := New(parent, matcher)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(detector)

	err = detector.watchDirectory(filepath.Join(parent, "vanished"))
	if !errors.Is(err, filepath.SkipDir) {
		t.Fatal("vanished directory reported as watch failure", err)
	}

	err = detector.watchDirectory(filepath.Join(file, "child"))
	if err == nil || errors.Is(err, filepath.SkipDir) {
		t.Fatal("real watch failure was hidden", err)
	}
}

func TestDirectoryWatchesDropsOnlyRenamedTree(t *testing.T) {
	t.Parallel()

	const (
		oldTree = "/source/old"
		nested  = "/source/old/nested"
	)

	watches := directoryWatches{
		oldTree:         {},
		nested:          {},
		"/source/other": {},
	}
	removed := make([]string, 0, 2)

	err := watches.dropTree(oldTree, func(path string) error {
		removed = append(removed, path)

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	slices.Sort(removed)

	if !slices.Equal(removed, []string{oldTree, nested}) {
		t.Fatal("unexpected watches removed", removed)
	}

	if _, kept := watches["/source/other"]; !kept || len(watches) != 1 {
		t.Fatal("unrelated watch was dropped", watches)
	}
}

func TestDirectoryWatchesForgetsStaleKernelEntries(t *testing.T) {
	t.Parallel()

	const oldTree = "/source/old"

	watches := directoryWatches{oldTree: {}, "/source/old/nested": {}}

	err := watches.dropTree(oldTree, func(path string) error {
		if path == oldTree {
			return fsnotify.ErrNonExistentWatch
		}

		return os.ErrPermission
	})
	if !errors.Is(err, os.ErrPermission) {
		t.Fatal("watch removal failure was lost", err)
	}

	if len(watches) != 0 {
		t.Fatal("stale watch bookkeeping was retained", watches)
	}
}

// A watch descriptor the kernel already invalidated (directory deleted or moved on a FUSE such as pmxcfs)
// makes inotify_rm_watch fail with EINVAL: the watch is gone, nothing is lost, the parent reported the event.
func TestDirectoryWatchesIgnoreInvalidatedDescriptors(t *testing.T) {
	t.Parallel()

	const oldTree = "/etc/pve/nodes/old"

	watches := directoryWatches{oldTree: {}, "/etc/pve/nodes/old/lxc": {}}

	err := watches.dropTree(oldTree, func(string) error {
		return fmt.Errorf("inotify_rm_watch: %w", unix.EINVAL)
	})
	if err != nil {
		t.Fatal("invalidated descriptor reported as coverage loss", err)
	}

	if len(watches) != 0 {
		t.Fatal("stale watch bookkeeping was retained", watches)
	}
}
