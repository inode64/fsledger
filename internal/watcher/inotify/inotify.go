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
	mounts mountStacks
	// rewalk asks restoreLoop to walk the root again after a loss; lossEpoch counts the losses consumed.
	rewalk chan struct{}
	path   string
	// workers runs readErrors and restoreLoop, which Close waits for.
	workers      sync.WaitGroup
	lossEpoch    atomic.Uint64
	coverage     sync.Mutex
	once         sync.Once
	coverageLost atomic.Bool
	// unwatchedFilesystem reports another filesystem mounted below the root, or a remote one on it.
	unwatchedFilesystem atomic.Bool
	// mountsStopped reports a mount follower that failed: no walk restores what it no longer follows.
	mountsStopped atomic.Bool
	// deepDirectories reports directories left unwatched because their paths reach PATH_MAX; deepMarks counts
	// the walks that found one.
	deepMarks       atomic.Uint64
	deepDirectories atomic.Bool
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
		rewalk:      make(chan struct{}, 1),
	}, nil
}

// Name identifies the selected backend.
func (*Watcher) Name() string { return event.Inotify }

// LossReason consumes the cause of the loss reported by Dirty.
func (w *Watcher) LossReason() string { return w.queue.LossReason() }

// Events exposes normalized raw notifications.
func (w *Watcher) Events() <-chan *event.Raw { return w.queue.Channel }

// Dirty consumes overflow/loss of coverage notifications. Directories created during a loss may lack a
// watch, so the reader walks the root again; until then reconciliation covers them.
func (w *Watcher) Dirty() bool {
	dirty := w.queue.Dirty()
	if dirty {
		// The epoch moves first: a walk ending in between must not clear the flag set for this loss.
		w.lossEpoch.Add(1)
		w.coverageLost.Store(true)

		select {
		case w.rewalk <- struct{}{}:
		default:
		}
	}

	return dirty
}

// PollingRequired covers watches potentially missed during an overflow or reader failure, and filesystems the
// watches cannot follow: another one mounted below the root after Start, as for a nested mount without a usable
// detector at startup, or a remote one on the root.
func (w *Watcher) PollingRequired() bool {
	return w.unwatchedFilesystem.Load() || w.mountsStopped.Load() || w.deepDirectories.Load() ||
		watcher.PollingRequired(w.done, &w.coverageLost)
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

	w.workers.Go(w.readErrors)
	w.workers.Go(func() { w.restoreLoop(ctx) })

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

		w.workers.Wait()
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

// addTree walks without following symlinks, even one swapped in meanwhile. Another filesystem mounted below
// the root, without a detector of its own, is a boundary that reconciliation covers.
func (w *Watcher) addTree(path string) error {
	marks := w.deepMarks.Load()
	deep, err := watcher.WalkDirectories(path, w.unwatched, w.watchDirectory)
	w.noteDeep(path, deep, marks)

	return fault.Wrap("install recursive watches", err)
}

// noteDeep records directories left unwatched because their paths reach PATH_MAX; reconciliation covers
// them. Only a walk of the root can tell that none remain, and only if no other walk found one since it
// began: marks is the count of such findings when it started.
func (w *Watcher) noteDeep(path string, deep bool, marks uint64) {
	switch {
	case deep:
		w.deepMarks.Add(1)
		w.deepDirectories.Store(true)
	case path == w.path && w.deepMarks.Load() == marks:
		w.deepDirectories.Store(false)
	}
}

// unwatched prunes excluded directories and other filesystems mounted below the root.
func (w *Watcher) unwatched(name string) bool {
	return w.matcher.MatchEntry(name) || w.mounts.boundary(w.path, name)
}

// watchDirectory watches name, which the walk opened as directory. A visit made stale by a later change of
// name is dropped without touching the registrations: the change has its own event. A name already watched
// for the same directory is left alone: adding it again after a symlink swap would make fsnotify keep a
// dangling entry, and removing that entry panics. After lost events the directory may still be registered
// under a name it left, or the name for a directory it replaced: both subtrees belong to directories that
// moved and are dropped first. inotify resolves name again and follows its symlinks, so a watch that landed
// elsewhere meanwhile is removed. A directory removed meanwhile has nothing left to watch: its parent's watch
// reports the removal, so ENOENT is not coverage loss and must not taint attribution.
//
//nolint:cyclop,gocognit,gocyclo // Each check guards one registration hazard listed above; split, they lose order.
func (w *Watcher) watchDirectory(name string, directory int) error {
	identity, err := watcher.IdentifyDescriptor(directory)
	if err != nil || !w.holds(name, identity) {
		return fs.SkipDir
	}

	if watched, known := w.directories.get(name); known && watched == identity {
		return nil
	}

	// A previous name still holding the directory is a bind mount, which the mount follower turns into a
	// boundary: its watches are still correct.
	if previous, moved := w.directories.nameOf(identity); moved {
		if w.holds(previous, identity) {
			return fs.SkipDir
		}

		err = w.dropTree(previous)
		if err != nil {
			return err
		}
	}

	if _, replaced := w.directories.get(name); replaced {
		err = w.dropTree(name)
		if err != nil {
			return err
		}
	}

	err = w.native.Add(name)
	if errors.Is(err, os.ErrNotExist) {
		return fs.SkipDir
	}

	if err != nil {
		return fmt.Errorf("watch %s: %w", name, err)
	}

	var resolved unix.Stat_t
	if unix.Stat(name, &resolved) != nil || resolved.Dev != identity.Device || resolved.Ino != identity.Inode {
		removeErr := w.native.Remove(name)
		if removeErr != nil && !staleWatch(removeErr) {
			return fmt.Errorf("remove misdirected watch %s: %w", name, removeErr)
		}

		return fs.SkipDir
	}

	w.directories.set(name, identity)

	return nil
}

// holds reports whether name, not followed, still names the directory identity describes.
func (*Watcher) holds(name string, identity watcher.DirectoryIdentity) bool {
	current, err := watcher.IdentifyPath(name)

	return err == nil && current == identity
}

// dropTree removes paths fsnotify still associates with the old name of a
// renamed directory. Descendant watches do not receive MOVE_SELF themselves.
func (w *Watcher) dropTree(path string) error {
	return fault.Wrap("remove stale recursive watches", w.directories.dropTree(path, w.native.Remove))
}

// directoryWatches records the directory each watched name holds and the name each directory is watched
// under, finding a removed tree's watches without scanning every watched directory.
type directoryWatches struct {
	identities map[watcher.DirectoryIdentity]string
	names      pathindex.Index[watcher.DirectoryIdentity]
}

func (directories *directoryWatches) get(name string) (watcher.DirectoryIdentity, bool) {
	return directories.names.Get(name)
}

func (directories *directoryWatches) nameOf(identity watcher.DirectoryIdentity) (string, bool) {
	name, found := directories.identities[identity]

	return name, found
}

func (directories *directoryWatches) set(name string, identity watcher.DirectoryIdentity) {
	directories.remove(name)

	if directories.identities == nil {
		directories.identities = make(map[watcher.DirectoryIdentity]string)
	}

	directories.names.Set(name, identity)
	directories.identities[identity] = name
}

func (directories *directoryWatches) remove(name string) {
	identity, found := directories.names.Get(name)
	if !found {
		return
	}

	directories.names.Delete(name)

	if directories.identities[identity] == name {
		delete(directories.identities, identity)
	}
}

func (directories *directoryWatches) dropTree(path string, remove func(string) error) error {
	var failure error

	for directory := range directories.names.Subtree(path) {
		err := remove(directory)
		if err != nil && !staleWatch(err) {
			failure = errors.Join(failure, err)
		}

		directories.remove(directory)
	}

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
		case native, ok := <-w.native.Events:
			if !ok {
				return
			}

			w.handle(native)
		}
	}
}

// readErrors reads fsnotify's unbuffered error channel apart from the reader, until Close closes it: fsnotify
// reports some errors while holding the lock its Add and Remove take, and the reader calls them.
func (w *Watcher) readErrors() {
	for err := range w.native.Errors {
		w.queue.Send(event.Raw{Dirty: true, Reason: err.Error(), Backend: w.Name()})
	}
}

// rewalkInterval spaces the walks that follow losses, which usually come in bursts.
const rewalkInterval = 30 * time.Second

// restoreLoop walks off the reader, which keeps draining events meanwhile: a walk blocking it during the
// burst that caused the loss would overflow the queues again.
func (w *Watcher) restoreLoop(ctx context.Context) {
	var last time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.rewalk:
		}

		wait := time.NewTimer(time.Until(last.Add(rewalkInterval)))

		select {
		case <-ctx.Done():
			wait.Stop()

			return
		case <-wait.C:
		}

		w.restoreCoverage(ctx)

		last = time.Now()
	}
}

// restoreCoverage watches directories created during a loss. Directories deleted during it lost their watches
// with their inodes, and fsnotify forgot them: the bookkeeping forgets them too, or a recreated directory
// reusing the inode number would count as watched. A failed walk, one stopped by Close, or a loss consumed
// meanwhile, keeps coverage lost.
func (w *Watcher) restoreCoverage(ctx context.Context) {
	epoch := w.lossEpoch.Load()

	var err error

	w.coverage.Lock()

	watched := make(map[string]bool)
	for _, name := range w.native.WatchList() {
		watched[name] = true
	}

	var forgotten []string

	for name := range w.directories.names.All() {
		if !watched[name] {
			forgotten = append(forgotten, name)
		}
	}

	for _, name := range forgotten {
		w.directories.remove(name)
	}

	w.coverage.Unlock()

	// The lock is taken per directory: held for the whole walk, it would stop the reader, which takes it for
	// every created or removed name, and overflow the queues again.
	if !w.matcher.Match(w.path) {
		var deep bool

		marks := w.deepMarks.Load()

		deep, err = watcher.WalkDirectories(w.path, func(name string) bool {
			w.coverage.Lock()
			defer w.coverage.Unlock()

			return w.unwatched(name)
		}, func(name string, directory int) error {
			if ctx.Err() != nil {
				return fault.Wrap("restore watches", ctx.Err())
			}

			w.coverage.Lock()
			defer w.coverage.Unlock()

			return w.watchDirectory(name, directory)
		})

		w.noteDeep(w.path, deep, marks)
	}

	if err == nil && w.lossEpoch.Load() == epoch {
		w.coverageLost.Store(false)

		// A loss consumed between the check and the store keeps coverage lost.
		if w.lossEpoch.Load() != epoch {
			w.coverageLost.Store(true)
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

// maintainCoverage takes the coverage lock only for names that may change watches: writes, and creations of
// anything but a directory, go straight through. A directory replacing such a name later has its own Create.
func (w *Watcher) maintainCoverage(native fsnotify.Event) {
	removed := native.Has(fsnotify.Remove) || native.Has(fsnotify.Rename)

	created := false

	// A name too long to stat may be a directory: addTree records it as left unwatched.
	if native.Has(fsnotify.Create) {
		var stat unix.Stat_t

		err := unix.Lstat(native.Name, &stat)
		created = err == nil && stat.Mode&unix.S_IFMT == unix.S_IFDIR || errors.Is(err, unix.ENAMETOOLONG)
	}

	if !removed && !created {
		return
	}

	w.coverage.Lock()
	defer w.coverage.Unlock()

	// A name removed or renamed that still holds the directory recorded for it, as after RENAME_EXCHANGE or a
	// rename back, is watched again: its Create came first and dropping the tree removes that watch, or fsnotify
	// dropped the watch of the moved directory itself. Another directory there has its own Create to come.
	if removed {
		recorded, known := w.directories.get(native.Name)
		created = created || known && w.holds(native.Name, recorded)

		w.reportCoverageError(w.dropTree(native.Name))
	}

	if created {
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
		// fsnotify delivers no event without an operation; a write is the most general change.
		return event.Write
	}
}
