package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/mirror"
	"github.com/inode64/fsledger/internal/watcher"
)

// queuedDetector models a native detector whose control requests stay outside Events.
type queuedDetector struct{ *watcher.Queue }

func (*queuedDetector) Start(context.Context) error        { return nil }
func (*queuedDetector) Close() error                       { return nil }
func (*queuedDetector) Name() string                       { return event.Fanotify }
func (detector *queuedDetector) Events() <-chan *event.Raw { return detector.Channel }

func TestOverflowInvalidatesPendingActor(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)

	path := writeSource(t, runner, "shared", "initial")

	err := runner.reconcile(t.Context(), "initial")
	if err != nil {
		t.Fatal(err)
	}

	writeSource(t, runner, "shared", "known writer")
	runner.accept(
		event.Raw{
			Path:  path,
			Actor: event.Actor{Known: true, UserKnown: true, PID: 123, StartTime: 456},
			Time:  time.Now(),
		},
	)
	writeSource(t, runner, "shared", "lost writer")

	queue := watcher.NewQueue(1)
	runner.queue = queue
	queue.Send(event.Raw{Path: "/irrelevant"})
	queue.Send(event.Raw{Path: path})
	runner.flush(t.Context(), time.Now().Add(time.Second))

	message := gitOutput(t, runner, "log", "-1", "--format=%B")
	if !strings.Contains(message, "Actor: unknown") || strings.Contains(message, "pid: 123") {
		t.Fatal(message)
	}

	relative, err := mirror.Relative(path)
	if err != nil {
		t.Fatal(err)
	}

	if got := gitOutput(t, runner, "show", "HEAD:"+relative); got != "lost writer" {
		t.Fatal(got)
	}
}

func TestRoutineRescanDoesNotEnablePermanentPolling(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	runner.cfg.Watch.Reconcile.Enabled = false
	queue := watcher.NewQueue(1)
	runner.watchers = []watcher.Watcher{&queuedDetector{Queue: queue}}
	path := writeSource(t, runner, "routine", "discovered by scan")

	queue.Send(event.Raw{Dirty: true, Reason: event.Reconciliation})
	runner.flush(t.Context(), time.Now())

	if runner.dirty || runner.periodicReconciliation() || runner.pendingError {
		t.Fatal("routine scan left permanent recovery state")
	}

	if got := gitOutput(t, runner, "show", "HEAD:"+strings.TrimPrefix(path, "/")); got != "discovered by scan" {
		t.Fatal("watcher reconciliation did not commit the source", got)
	}
}

func TestExplicitWatcherLossIsReportedOnce(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	queue := watcher.NewQueue(1)
	runner.watchers = []watcher.Watcher{&queuedDetector{Queue: queue}}
	queue.Send(event.Raw{Dirty: true, Reason: "kernel loss"})
	runner.checkLosses()
	runner.checkLosses()

	if !runner.contaminated || len(runner.status.Warnings) != 1 ||
		strings.Contains(runner.status.Warnings[0], "queue overflow") ||
		!strings.Contains(runner.status.Warnings[0], "kernel loss") {
		t.Fatal(runner.status.Warnings)
	}
}

func TestShutdownDeadlineIsRecoverableButOtherErrorsRemain(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := runner.finishShutdown(ctx, ctx, context.Canceled)
	if err != nil || runner.status.Running {
		t.Fatal(err)
	}

	failure := os.ErrPermission

	err = runner.finishShutdown(ctx, ctx, failure)
	if !errors.Is(err, failure) {
		t.Fatal("real failure hidden", err)
	}
}

func TestStatusSnapshotDoesNotFollowExistingLinks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	victim := filepath.Join(t.TempDir(), "victim")

	err := os.WriteFile(victim, []byte("untouched"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"repo.json", "repo.json.tmp"} {
		err = os.Symlink(victim, filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
	}

	err = writeStatus(root, "repo.json", []byte(`{"running":false}`))
	if err != nil {
		t.Fatal(err)
	}

	//nolint:gosec // The victim is a fixed file inside this test's private temporary directory.
	data, err := os.ReadFile(victim)
	if err != nil || string(data) != "untouched" {
		t.Fatal(string(data), err)
	}

	info, err := os.Lstat(filepath.Join(root, "repo.json"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatal(info, err)
	}
}

// limitedDetector is a fanotify group whose last walk of the root hit the mark limit.
type limitedDetector struct{ queuedDetector }

func (*limitedDetector) MarkLimitReached() bool { return true }

// Directories beyond the mark limit are a lasting advisory, not a coverage loss to recover from.
func TestMarkLimitIsAdvisedNotLost(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	runner.watchers = []watcher.Watcher{&limitedDetector{queuedDetector{Queue: watcher.NewQueue(1)}}}

	runner.checkLosses()

	if runner.contaminated || runner.pendingError || !slices.Contains(runner.status.Warnings, fanotifyMarkLimit) {
		t.Fatal("mark limit reported as loss or not advised", runner.status.Warnings)
	}
}

// Events a full queue dropped are observed again without an actor, not as a loss forcing a full scan.
func TestDroppedEventsDowngradeOnlyTheirPaths(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)

	err := runner.reconcile(t.Context(), "initial")
	if err != nil {
		t.Fatal(err)
	}

	runner.dirty = false
	known := event.Actor{
		Known: true, PID: 7, StartTime: 123, Executable: "/usr/bin/editor", UserKnown: true, UID: 1000, EUID: 1000,
	}
	shared, other := writeSource(t, runner, "shared", "edited"), writeSource(t, runner, "other", "edited")

	for _, path := range []string{shared, other} {
		runner.accept(event.Raw{Path: path, Operation: event.Write, Backend: event.Inotify, Actor: known})
	}

	queue := watcher.NewRememberingQueue(1, 8)
	runner.queue = queue
	// The first event fills the queue; the second is dropped.
	queue.Send(event.Raw{Path: other, Operation: event.Write, Actor: known})
	queue.Send(event.Raw{Path: shared, Operation: event.Write, Actor: known})

	runner.checkLosses()

	if runner.contaminated || runner.dirty {
		t.Fatal("dropped events were treated as a loss")
	}

	for _, group := range runner.manager.DueWithAttributionDelay(time.Now(), true, 0) {
		_, hasShared := group.Paths[shared]
		_, hasOther := group.Paths[other]

		if hasShared && group.Actor.Known || hasOther && !group.Actor.Known {
			t.Fatalf("dropped event changed the wrong actor: %+v", group)
		}
	}
}
