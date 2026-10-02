package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/mirror"
	"github.com/inode64/fsledger/internal/watcher"
)

// queuedDetector models a native detector whose control requests stay outside Events.
type queuedDetector struct{ *watcher.Queue }

func (*queuedDetector) Start(context.Context) error       { return nil }
func (*queuedDetector) Close() error                      { return nil }
func (*queuedDetector) Name() string                      { return event.Fanotify }
func (detector *queuedDetector) Events() <-chan event.Raw { return detector.Channel }

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
