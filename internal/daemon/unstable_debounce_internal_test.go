package daemon

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/grouping"
)

// A due unstable-path retry waits for pending groups as a scheduled reconciliation does: draining them
// would cut commit.debounce, and with Audit its correlation delay, short.
func TestUnstableRetryKeepsPendingGroupsDebounced(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	runner.cfg.Commit.Debounce, runner.cfg.Commit.MaxDelay = time.Hour, 2*time.Hour
	runner.manager = grouping.New(runner.cfg.Commit.Grouping, time.Hour, 2*time.Hour)

	file := writeSource(t, runner, "file", "before")

	err := runner.reconcile(t.Context(), "first")
	if err != nil {
		t.Fatal(err)
	}

	writeSource(t, runner, "file", "after")

	now := time.Now()
	actor := event.Actor{
		PID: 10, StartTime: 100, UID: 4242, LoginUID: 4242, UserKnown: true, LoginKnown: true, Known: true,
	}
	runner.accept(event.Raw{Path: file, Actor: actor, Backend: event.Fanotify, Operation: event.Write, Time: now})
	runner.unstable = map[string]struct{}{filepath.Join(runner.roots[0], "elsewhere"): {}}
	runner.unstableRetry = now.Add(-time.Second)

	runner.flush(t.Context(), now.Add(time.Second))

	if groups, _ := runner.manager.Pending(); groups != 1 ||
		strings.Contains(gitOutput(t, runner, "log", "--format=%B"), "4242") {
		t.Fatal("an unstable retry committed a group before its debounce", groups)
	}

	runner.flush(t.Context(), now.Add(3*time.Hour))

	if groups, _ := runner.manager.Pending(); groups != 0 || runner.status.Scan.Started.Before(now) ||
		!strings.Contains(gitOutput(t, runner, "log", "--format=%B"), "4242") {
		t.Fatal("the unstable retry never ran", groups, runner.status.Scan.Started)
	}
}
