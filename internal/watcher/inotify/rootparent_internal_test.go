package inotify

import (
	"os"
	"path/filepath"
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

	detector.restoreCoverage()

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
