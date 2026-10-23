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
	"github.com/inode64/fsledger/internal/watcher"
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

// A directory removed between the walk listing it and its watch installation is not coverage loss; a watch
// that cannot be installed on a directory still there is.
func TestVanishedDirectoryIsNotCoverageLoss(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector, err := New(parent, matcher)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(detector)

	opened, err := os.Open(parent) //nolint:gosec // The test's own temporary directory.
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(opened)

	err = detector.watchDirectory(filepath.Join(parent, "vanished"), int(opened.Fd()))
	if !errors.Is(err, filepath.SkipDir) {
		t.Fatal("vanished directory reported as watch failure", err)
	}

	// A closed instance refuses every watch, as an exhausted max_user_watches would.
	err = detector.native.Close()
	if err != nil {
		t.Fatal(err)
	}

	err = detector.watchDirectory(parent, int(opened.Fd()))
	if err == nil || errors.Is(err, filepath.SkipDir) {
		t.Fatal("real watch failure was hidden", err)
	}
}

// A name that no longer leads to the directory the walk opened, as after a symlink swap, keeps no watch.
func TestMisdirectedWatchIsRemoved(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()

	for _, name := range []string{"listed", "elsewhere"} {
		err := os.Mkdir(filepath.Join(parent, name), 0o700)
		if err != nil {
			t.Fatal(err)
		}
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

	//nolint:gosec // The test created this directory in its own temporary tree.
	opened, err := os.Open(filepath.Join(parent, "elsewhere"))
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(opened)

	listed := filepath.Join(parent, "listed")

	err = detector.watchDirectory(listed, int(opened.Fd()))
	if !errors.Is(err, filepath.SkipDir) || slices.Contains(detector.native.WatchList(), listed) {
		t.Fatal("watch on another directory than the opened one was kept", err, detector.native.WatchList())
	}

	if _, watched := detector.directories.get(listed); watched {
		t.Fatal("misdirected watch recorded")
	}
}

func TestDirectoryWatchesDropsOnlyRenamedTree(t *testing.T) {
	t.Parallel()

	const (
		oldTree = "/source/old"
		nested  = "/source/old/nested"
	)

	watches := watchesOf(oldTree, nested, "/source/other")
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

	if _, kept := watches.get("/source/other"); !kept || len(watches.names.Subtree("/")) != 1 {
		t.Fatal("unrelated watch was dropped", watches)
	}
}

func TestDirectoryWatchesForgetsStaleKernelEntries(t *testing.T) {
	t.Parallel()

	const oldTree = "/source/old"

	watches := watchesOf(oldTree, "/source/old/nested")

	err := watches.dropTree(oldTree, func(path string) error {
		if path == oldTree {
			return fsnotify.ErrNonExistentWatch
		}

		return os.ErrPermission
	})
	if !errors.Is(err, os.ErrPermission) {
		t.Fatal("watch removal failure was lost", err)
	}

	if len(watches.names.Subtree("/")) != 0 {
		t.Fatal("stale watch bookkeeping was retained", watches)
	}
}

// A watch descriptor the kernel already invalidated (directory deleted or moved on a FUSE such as pmxcfs)
// makes inotify_rm_watch fail with EINVAL: the watch is gone, nothing is lost, the parent reported the event.
func TestDirectoryWatchesIgnoreInvalidatedDescriptors(t *testing.T) {
	t.Parallel()

	const oldTree = "/etc/pve/nodes/old"

	watches := watchesOf(oldTree, "/etc/pve/nodes/old/lxc")

	err := watches.dropTree(oldTree, func(string) error {
		return fmt.Errorf("inotify_rm_watch: %w", unix.EINVAL)
	})
	if err != nil {
		t.Fatal("invalidated descriptor reported as coverage loss", err)
	}

	if len(watches.names.Subtree("/")) != 0 {
		t.Fatal("stale watch bookkeeping was retained", watches)
	}
}

func watchesOf(paths ...string) *directoryWatches {
	watches := &directoryWatches{}
	for index, path := range paths {
		watches.set(path, watcher.DirectoryIdentity{Device: 0, Inode: uint64(index + 1), Birth: 0})
	}

	return watches
}

// Watching again a name swapped for a symlink to another watched directory must neither panic in fsnotify
// nor move the watch.
func TestSwappedWatchedNameDoesNotPanic(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	listed, other := filepath.Join(parent, "listed"), filepath.Join(parent, "other")

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector, err := New(parent, matcher)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(detector)

	opened := make(map[string]*os.File)

	for _, name := range []string{listed, other} {
		err = os.Mkdir(name, 0o700)
		if err != nil {
			t.Fatal(err)
		}

		//nolint:gosec // The test created this directory in its own temporary tree.
		opened[name], err = os.Open(name)
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { resource.Close(opened[name]) })

		err = detector.watchDirectory(name, int(opened[name].Fd()))
		if err != nil {
			t.Fatal(err)
		}
	}

	err = os.Rename(listed, filepath.Join(parent, "moved"))
	if err == nil {
		err = os.Symlink(other, listed)
	}

	if err != nil {
		t.Fatal(err)
	}

	// The name no longer holds the opened directory: the visit is stale and changes nothing.
	err = detector.watchDirectory(listed, int(opened[listed].Fd()))
	if !errors.Is(err, filepath.SkipDir) {
		t.Fatal("stale visit not skipped", err)
	}

	err = detector.dropTree(listed)
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(detector.native.WatchList(), other) {
		t.Fatal("the other directory lost its watch")
	}
}

// A restore walk's visit with a descriptor opened before the reader handled a replacement must leave the
// reader's watch for the new directory alone.
func TestStaleWalkVisitKeepsNewerWatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	name := filepath.Join(root, "n")

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector, err := New(root, matcher)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(detector)

	err = os.Mkdir(name, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	stale, err := os.Open(name) //nolint:gosec // The test's own temporary directory.
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(stale)

	err = os.Rename(name, filepath.Join(root, "old"))
	if err == nil {
		err = os.Mkdir(name, 0o700)
	}

	if err == nil {
		err = detector.addTree(name)
	}

	if err != nil {
		t.Fatal(err)
	}

	err = detector.watchDirectory(name, int(stale.Fd()))
	if !errors.Is(err, filepath.SkipDir) || !slices.Contains(detector.native.WatchList(), name) {
		t.Fatal("stale visit removed the newer watch", err, detector.native.WatchList())
	}
}
