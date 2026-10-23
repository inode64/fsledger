// Package inotify adapts fsnotify to recursive, backend-independent events.
package inotify

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/inode64/fsledger/internal/pathindex"
	"github.com/inode64/fsledger/internal/pathutil"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/mountinfo"
	"github.com/inode64/fsledger/internal/resource"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/watcher"
)

// Watcher keeps recursive directory watches, including parents of configured roots.
type Watcher struct {
	native  *fsnotify.Watcher
	matcher *exclude.Matcher
	queue   *watcher.Queue
	done    chan struct{}
	// changes follows the mount table from Start, when the root is watched; mountsDone closes with its goroutine.
	changes     *mountinfo.Changes
	mountsDone  chan struct{}
	cancel      context.CancelFunc
	directories directoryWatches
	// mounts is the last table seen at or below the root; coverage guards it with the watches.
	mounts       mountStacks
	path         string
	coverage     sync.Mutex
	once         sync.Once
	coverageLost atomic.Bool
	// unwatchedFilesystem reports another filesystem mounted below the root, or a remote one on it.
	unwatchedFilesystem atomic.Bool
}

// New allocates an inotify instance; Start installs coverage.
func New(path string, matcher *exclude.Matcher) (*Watcher, error) {
	native, err := fsnotify.NewBufferedWatcher(watcher.QueueSize)
	if err != nil {
		return nil, fmt.Errorf("inotify init: %w", err)
	}

	return &Watcher{
		native:      native,
		path:        path,
		directories: directoryWatches{},
		matcher:     matcher,
		queue:       watcher.NewQueue(watcher.QueueSize),
		done:        make(chan struct{}),
		mountsDone:  make(chan struct{}),
	}, nil
}

// Name identifies the selected backend.
func (*Watcher) Name() string { return event.Inotify }

// LossReason consumes the cause of the loss reported by Dirty.
func (w *Watcher) LossReason() string { return w.queue.LossReason() }

// Events exposes normalized raw notifications.
func (w *Watcher) Events() <-chan *event.Raw { return w.queue.Channel }

// Dirty consumes overflow/loss of coverage notifications.
func (w *Watcher) Dirty() bool {
	dirty := w.queue.Dirty()
	if dirty {
		w.coverageLost.Store(true)
	}

	return dirty
}

// PollingRequired covers watches potentially missed during an overflow or reader failure, and filesystems the
// watches cannot follow: another one mounted below the root after Start, as for a nested mount without a usable
// detector at startup, or a remote one on the root.
func (w *Watcher) PollingRequired() bool {
	return w.unwatchedFilesystem.Load() || watcher.PollingRequired(w.done, &w.coverageLost)
}

// Start installs watches before beginning the initial reconciliation.
func (w *Watcher) Start(ctx context.Context) error {
	err := w.native.Add(filepath.Dir(w.path))
	if err != nil {
		return fmt.Errorf("watch root parent: %w", err)
	}

	// Events reach addTree only after handle applied the exclusions; the root has not been checked.
	if !w.matcher.Match(w.path) {
		err = w.watchTree()
		if err != nil {
			return err
		}
	}

	ctx, w.cancel = context.WithCancel(ctx)
	go w.read(ctx)

	if w.changes != nil {
		go w.followMounts(ctx)
	}

	return nil
}

// Close cancels the reader and releases all kernel watches.
func (w *Watcher) Close() error {
	var err error

	w.once.Do(func() {
		if w.cancel != nil {
			w.cancel()
		}

		// The mount follower installs watches: it stops before they are released.
		if w.changes != nil {
			w.changes.Stop()
			<-w.mountsDone
			err = w.changes.Close()
		}

		err = errors.Join(err, w.native.Close())
		if w.cancel != nil {
			<-w.done
		}
	})

	return fault.Wrap("close inotify", err)
}

// watchTree follows the mount table before reading it and walking the root: a mount meanwhile is still reported.
func (w *Watcher) watchTree() error {
	changes, err := mountinfo.FollowChanges()
	if err != nil {
		return err
	}

	mounts, err := mountinfo.Read()
	if err == nil {
		w.mounts = stackMounts(w.path, w.matcher, mounts)
		w.unwatchedFilesystem.Store(w.mounts.unwatched(w.path))
		err = w.addTree(w.path)
	}

	if err != nil {
		resource.Close(changes)

		return err
	}

	w.changes = changes

	return nil
}

func (w *Watcher) addTree(path string) error {
	err := filepath.WalkDir(path, func(name string, entry fs.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		if err != nil {
			return err
		}

		if !entry.IsDir() {
			return nil
		}

		// Another filesystem mounted below the root, without a detector of its own, is a boundary that
		// reconciliation covers.
		if w.matcher.MatchEntry(name) || w.mounts.boundary(w.path, name) {
			return filepath.SkipDir
		}

		return w.watchDirectory(name)
	})

	return fault.Wrap("install recursive watches", err)
}

// A directory removed after the walk listed it has nothing left to watch: its parent
// watch reports the removal, so ENOENT is not coverage loss and must not taint attribution.
func (w *Watcher) watchDirectory(name string) error {
	err := w.native.Add(name)
	if errors.Is(err, os.ErrNotExist) {
		return filepath.SkipDir
	}

	if err != nil {
		return fmt.Errorf("watch %s: %w", name, err)
	}

	w.directories.add(name)

	return nil
}

// dropTree removes paths fsnotify still associates with the old name of a
// renamed directory. Descendant watches do not receive MOVE_SELF themselves.
func (w *Watcher) dropTree(path string) error {
	return fault.Wrap("remove stale recursive watches", w.directories.dropTree(path, w.native.Remove))
}

// directoryWatches finds a removed tree's watches without scanning every watched directory.
type directoryWatches struct{ pathindex.Index[struct{}] }

func (directories *directoryWatches) add(path string) { directories.Set(path, struct{}{}) }

func (directories *directoryWatches) dropTree(path string, remove func(string) error) error {
	var failure error

	for directory := range directories.Subtree(path) {
		err := remove(directory)
		if err != nil && !staleWatch(err) {
			failure = errors.Join(failure, err)
		}
	}

	directories.DeleteSubtree(path)

	return failure
}

// staleWatch reports a removal that found nothing to remove: fsnotify no longer knows the path, or the kernel
// already invalidated the descriptor (EINVAL from inotify_rm_watch: the directory was deleted or moved, as
// pmxcfs does under /etc/pve). Its parent watch reported that event, so coverage was not lost.
func staleWatch(err error) bool {
	return errors.Is(err, fsnotify.ErrNonExistentWatch) || errors.Is(err, unix.EINVAL)
}

func (w *Watcher) read(ctx context.Context) {
	defer close(w.done)

	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-w.native.Errors:
			if !ok {
				return
			}

			w.queue.Send(event.Raw{Dirty: true, Reason: err.Error(), Backend: w.Name()})
		case native, ok := <-w.native.Events:
			if !ok {
				return
			}

			w.handle(native)
		}
	}
}

func (w *Watcher) handle(native fsnotify.Event) {
	// fsnotify appends names to the watch path after a slash: the "/" watch over a root such as /etc reports "//etc".
	native.Name = filepath.Clean(native.Name)

	if !pathutil.Contains(w.path, native.Name) || w.matcher.Match(native.Name) {
		return
	}

	raw := event.Raw{Path: native.Name, Backend: w.Name(), Time: time.Now(), Operation: operation(native.Op)}
	w.maintainCoverage(native)
	w.queue.Send(raw)
}

func (w *Watcher) maintainCoverage(native fsnotify.Event) {
	w.coverage.Lock()
	defer w.coverage.Unlock()

	if native.Has(fsnotify.Remove) || native.Has(fsnotify.Rename) {
		w.reportCoverageError(w.dropTree(native.Name))
	}

	if native.Has(fsnotify.Create) {
		w.reportCoverageError(w.addTree(native.Name))
	}
}

func (w *Watcher) reportCoverageError(err error) {
	if err != nil {
		w.queue.Send(event.Raw{Dirty: true, Reason: err.Error(), Backend: w.Name()})
	}
}

func operation(operationCode fsnotify.Op) string {
	switch {
	case operationCode.Has(fsnotify.Remove), operationCode.Has(fsnotify.Rename):
		return event.Remove
	case operationCode.Has(fsnotify.Create):
		return event.Create
	case operationCode.Has(fsnotify.Write):
		return event.Write
	case operationCode.Has(fsnotify.Chmod):
		return event.Attrib
	default:
		return "UNKNOWN"
	}
}
