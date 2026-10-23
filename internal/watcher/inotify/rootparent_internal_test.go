package inotify

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/resource"
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

	detector.directories.add(root)
	detector.directories.add(filepath.Join(root, "ssh"))

	detector.handle(fsnotify.Event{Name: "//etc", Op: fsnotify.Remove})

	select {
	case raw := <-detector.Events():
		if raw.Dirty || raw.Path != root || raw.Operation != event.Remove {
			t.Fatal("root removal reported under the wrong path", raw)
		}
	default:
		t.Fatal("root removal was dropped")
	}

	if len(detector.directories.Subtree("/")) != 0 {
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

	if _, watched := detector.directories.Get(root); !watched || len(detector.directories.Subtree("/")) != 2 {
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
