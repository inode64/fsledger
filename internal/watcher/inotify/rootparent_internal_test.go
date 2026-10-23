package inotify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/resource"
	"github.com/inode64/fsledger/internal/watcher"
)

// The "/" watch over a root such as /etc names its events "//etc": the removal of the root itself must still be
// reported under the canonical path and drop its recursive watches.
func TestRootParentRemovalIsCanonical(t *testing.T) {
	t.Parallel()

	const root = "/etc"

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector, err := New(root, matcher)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(detector)

	detector.directories.set(root, watcher.DirectoryIdentity{Device: 0, Inode: 1, Birth: 0})
	detector.directories.set(filepath.Join(root, "ssh"), watcher.DirectoryIdentity{Device: 0, Inode: 2, Birth: 0})

	detector.handle(fsnotify.Event{Name: "//etc", Op: fsnotify.Remove})

	select {
	case raw := <-detector.Events():
		if raw.Dirty || raw.Path != root || raw.Operation != event.Remove {
			t.Fatal("root removal reported under the wrong path", raw)
		}
	default:
		t.Fatal("root removal was dropped")
	}

	if len(detector.directories.names.Subtree("/")) != 0 {
		t.Fatal("stale root watches were retained", detector.directories)
	}
}

// A recreated root reported with the doubled leading slash of the "/" watch must recover its recursive watches.
func TestRootParentCreationRestoresCoverage(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "root")

	err := os.MkdirAll(filepath.Join(root, "nested"), 0o700)
	if err != nil {
		t.Fatal(err)
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

	detector.handle(fsnotify.Event{Name: "/" + root, Op: fsnotify.Create})

	if got := detector.native.WatchList(); len(got) != 2 {
		t.Fatal("lost root recreation coverage", got)
	}

	if _, watched := detector.directories.get(root); !watched || len(detector.directories.names.Subtree("/")) != 2 {
		t.Fatal("root watch recorded under a non-canonical path", detector.directories)
	}
}

// A consumed loss makes the reader walk the root again: coverage recovers without a restart.
func TestCoverageRecoversAfterLoss(t *testing.T) {
	t.Parallel()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector, err := New(t.TempDir(), matcher)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(detector)

	err = detector.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	detector.queue.Send(event.Raw{Dirty: true, Reason: "inotify queue overflow", Backend: event.Inotify})

	if !detector.Dirty() {
		t.Fatal("loss not reported")
	}

	deadline := time.Now().Add(5 * time.Second)
	for detector.PollingRequired() {
		if time.Now().After(deadline) {
			t.Fatal("coverage stayed lost after the walk")
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// A failed mount follower is not restored by any walk: reconciliation must keep covering mounts.
func TestStoppedMountFollowerKeepsPolling(t *testing.T) {
	t.Parallel()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector, err := New(t.TempDir(), matcher)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(detector)

	detector.stopFollowing(nil)

	if detector.PollingRequired() {
		t.Fatal("a closed follower forced polling")
	}

	detector.stopFollowing(os.ErrDeadlineExceeded)

	if !detector.PollingRequired() || !detector.Dirty() {
		t.Fatal("a failed follower was not reported")
	}

	detector.restoreCoverage(t.Context())

	if !detector.PollingRequired() {
		t.Fatal("a walk cleared the failed follower")
	}
}

// A directory created past PATH_MAX cannot be watched or even stat'ed: reconciliation must cover it.
func TestCreationPastPathMaxForcesPolling(t *testing.T) {
	t.Parallel()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()

	detector, err := New(root, matcher)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(detector)

	name := filepath.Join(root, strings.Repeat(strings.Repeat("d", 250)+"/", 17), "new")
	detector.maintainCoverage(fsnotify.Event{Name: name, Op: fsnotify.Create})

	if !detector.PollingRequired() {
		t.Fatal("a directory past PATH_MAX was left uncovered")
	}
}

// Close waits for a running restore walk, which stops at the next directory once the watcher closes.
func TestRestoreWalkStopsWithItsContext(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	err := os.MkdirAll(filepath.Join(root, "nested"), 0o700)
	if err != nil {
		t.Fatal(err)
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

	detector.coverageLost.Store(true)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	detector.restoreCoverage(ctx)

	if got := detector.native.WatchList(); len(got) != 0 || !detector.coverageLost.Load() {
		t.Fatal("a cancelled walk went on, or restored coverage:", got)
	}
}

// fsnotify reports a watch it fails to remove after a move while holding the lock its Add and Remove take: the
// error must be read while the reader waits for that lock, or neither ever proceeds.
func TestErrorsDrainWhileTheReaderWaits(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	moved := filepath.Join(root, "moved")

	err := os.Mkdir(moved, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	detector := startedWatcher(t.Context(), t, root)

	// The reader waits for the lock with the directory created first; fsnotify reads the move only after the
	// moved directory is gone, and fails to remove its watch.
	detector.coverage.Lock()
	defer detector.coverage.Unlock()

	err = os.Mkdir(filepath.Join(root, "created"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	fillEvents(t, detector, root)

	err = errors.Join(os.Rename(moved, moved+".old"), os.Remove(moved+".old"))
	if err != nil {
		t.Fatal(err)
	}

	timeout := time.After(5 * time.Second)

	for removed := false; !removed; {
		select {
		case native := <-detector.native.Events:
			removed = native.Name == moved+".old" && native.Has(fsnotify.Remove)
		case <-timeout:
			failStopped(t, detector)
		}
	}

	if !eventually(detector.Dirty) {
		t.Fatal("the fsnotify error was not reported as a loss")
	}
}

// Before Start, as while it installs the first watches, nobody reads fsnotify's events, yet the error it reports
// for a watch it fails to remove after a move must still be read, or its next Add waits forever.
func TestErrorsDrainBeforeStart(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	moved := filepath.Join(root, "moved")

	err := os.Mkdir(moved, 0o700)
	if err != nil {
		t.Fatal(err)
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

	err = errors.Join(detector.native.Add(root), detector.native.Add(moved))
	if err != nil {
		t.Fatal(err)
	}

	// fsnotify reads the move only after the moved directory is gone, and fails to remove its watch.
	fillEvents(t, detector, root)

	err = errors.Join(os.Rename(moved, moved+".old"), os.Remove(moved+".old"))
	if err != nil {
		t.Fatal(err)
	}

	added := make(chan error, 1)

	go func() {
		for native := range detector.native.Events {
			if native.Name == moved+".old" && native.Has(fsnotify.Remove) {
				break
			}
		}

		added <- detector.native.Add(root)
	}()

	select {
	case err = <-added:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		failStopped(t, detector)
	}
}

// failStopped fails a test whose fsnotify stopped; reading its pending error lets fsnotify, and so the cleanup,
// go on.
func failStopped(t *testing.T, detector *Watcher) {
	t.Helper()

	select {
	case err := <-detector.native.Errors:
		t.Fatal("fsnotify stopped on an error nobody read:", err)
	default:
		t.Fatal("no event for the removed directory")
	}
}

// fillEvents creates files under root until fsnotify's channel is full: fsnotify then reads no event meanwhile.
func fillEvents(t *testing.T, detector *Watcher, root string) {
	t.Helper()

	for name := range watcher.QueueSize + 1 {
		writeFile(t, filepath.Join(root, strconv.Itoa(name)))
	}

	if !eventually(func() bool { return len(detector.native.Events) == cap(detector.native.Events) }) {
		t.Fatal("fsnotify's channel did not fill")
	}
}

// eventually reports whether condition holds within five seconds.
func eventually(condition func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			return false
		}

		time.Sleep(time.Millisecond)
	}

	return true
}
