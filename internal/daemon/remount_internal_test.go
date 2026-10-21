package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/mirror"
	"github.com/inode64/fsledger/internal/resource"
	"github.com/inode64/fsledger/internal/watcher"
	"github.com/inode64/fsledger/internal/watcher/fanotify"
	"github.com/inode64/fsledger/internal/watcher/inotify"
)

// No fanotify mark sees a mount either: taking inotify's reconciliation request for a remounted directory for a
// duplicate of fanotify's directory events would drop it whenever fanotify watches the same root.
func TestRemountRequestIsNotFanotifyCoverage(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector, err := fanotify.New(root, matcher)
	if err != nil {
		t.Skipf("fanotify unavailable: %v", err)
	}
	defer resource.Close(detector)

	if detector.Mode() != fanotify.ModeDFIDName {
		t.Skipf("directory name handles unavailable: mode=%s", detector.Mode())
	}

	err = detector.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	runner := &worker{watchers: []watcher.Watcher{detector}}
	mounted := filepath.Join(root, "mounted")

	if !runner.nativeCovers(event.Raw{Backend: event.Inotify, Path: mounted}) {
		t.Fatal("fanotify does not cover the entries of its root")
	}

	if runner.nativeCovers(event.Raw{Backend: event.Inotify, Path: mounted, Reason: inotify.RemountReason}) {
		t.Fatal("the remount request was dropped as a duplicate of fanotify's events")
	}
}

// The request reconciles the remounted directory: none of the files the new filesystem shows raised an event.
func TestRemountRequestReconcilesTheDirectory(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	mounted := filepath.Join(runner.status.Paths[0], "mounted")

	err := os.MkdirAll(filepath.Join(mounted, "inner"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = runner.reconcile(t.Context(), "Initial snapshot")
	if err != nil {
		t.Fatal(err)
	}

	path := writeSource(t, runner, filepath.Join("mounted", "inner", "file"), "shown by the mount")
	now := time.Now()
	runner.accept(
		event.Raw{
			Path:      mounted,
			Backend:   event.Inotify,
			Operation: event.Create,
			Reason:    inotify.RemountReason,
			Time:      now,
		},
	)
	runner.flush(t.Context(), now.Add(time.Second))

	relative, err := mirror.Relative(path)
	if err != nil {
		t.Fatal(err)
	}

	if got := gitOutput(t, runner, "show", "HEAD:"+relative); got != "shown by the mount" {
		t.Fatal("the remounted directory was not reconciled", got)
	}

	if message := gitOutput(t, runner, "log", "-1", "--format=%B"); !strings.Contains(message, inotify.RemountReason) {
		t.Fatal("the commit does not name the mount table change", message)
	}
}
