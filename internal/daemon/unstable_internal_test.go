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
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/mirror"
)

func checkGitContent(t *testing.T, runner *worker, path, want string) {
	t.Helper()

	relative, err := mirror.Relative(path)
	if err != nil {
		t.Fatal(err)
	}

	if got := gitOutput(t, runner, "show", "HEAD:"+relative); got != want {
		t.Fatalf("Git content %q, want %q", got, want)
	}
}

//nolint:cyclop,funlen,gocyclo // Real Git fixture covers partial scans, actor commits and timer-only recovery.
func TestUnstableInventoryDoesNotBlockGitOrContaminateActors(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	runner.cfg.Watch.Reconcile.Enabled = false
	hot := writeSource(t, runner, "hot", "original")
	stable := writeSource(t, runner, "stable", "original")

	err := runner.reconcile(t.Context(), "Initial snapshot")
	if err != nil {
		t.Fatal(err)
	}

	writeSource(t, runner, "hot", "changed")
	writeSource(t, runner, "stable", "stable change")

	mutations := 0

	var mutationErr error

	runner.catalog.BindCopiedHashes(func(record integrity.Record, _ string) (string, int64, bool) {
		if string(record.Path) == hot {
			mutations++
			mutationErr = os.WriteFile(hot, make([]byte, mutations), 0o600)
		}

		return "", 0, false
	})

	runner.dirty = true
	runner.flush(t.Context(), time.Now())

	if runner.contaminated || runner.dirty || runner.pendingError || len(runner.unstable) != 1 || mutationErr != nil {
		t.Fatal("partial reconciliation became an operational failure", runner.status, mutationErr)
	}

	checkGitContent(t, runner, stable, "stable change")

	if len(runner.status.UnstablePaths) != 1 || runner.periodicReconciliation() {
		t.Fatal("pending path not visible or periodic reconciliation unexpectedly enabled")
	}

	writeSource(t, runner, "stable", "next actor")

	now := time.Now()
	runner.accept(event.Raw{
		Path: stable, Backend: event.Fanotify, Time: now,
		Actor: event.Actor{
			PID: 123, StartTime: 100, UID: 1000, UserKnown: true, Known: true, Executable: "/usr/bin/stable-writer",
		},
	})
	runner.flush(t.Context(), now.Add(time.Second))
	checkGitContent(t, runner, stable, "next actor")

	if runner.contaminated || !strings.Contains(gitOutput(t, runner, "log", "-1", "--format=%B"), "stable-writer") {
		t.Fatal("unrelated actor lost during partial reconciliation")
	}

	runner.catalog.BindCopiedHashes(runner.mirror.CopiedHash)
	writeSource(t, runner, "hot", "settled without event")
	runner.unstableRetry = time.Now().Add(-time.Second)
	runner.flush(t.Context(), time.Now())
	checkGitContent(t, runner, hot, "settled without event")

	if len(runner.unstable) != 0 || runner.contaminated || runner.pendingError ||
		len(runner.status.UnstablePaths) != 0 {
		t.Fatal("deferred observation not recovered", runner.status)
	}

	if strings.Contains(gitOutput(t, runner, "log", "-1", "--format=%B"), "stable-writer") {
		t.Fatal("retry attributed to unrelated actor")
	}

	pending, err := runner.catalog.PendingIntents(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatal("completed recovery left intents", pending, err)
	}
}

func TestUnstableDoesNotMaskFatalError(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)

	err := runner.deferUnstable(errors.Join(integrity.UnstablePathsError{"/unstable"}, context.Canceled))
	if !errors.Is(err, context.Canceled) || len(runner.unstable) != 1 || runner.pendingError {
		t.Fatal(err, runner.unstable)
	}

	runner.failedReconciliation(err)

	if !runner.contaminated || !runner.pendingError {
		t.Fatal("operational failure no longer contaminates")
	}
}

func TestFlushRequestWaitsForUnstablePaths(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)

	err := RequestFlush(runner.cfg, runner.name)
	if err != nil {
		t.Fatal(err)
	}

	err = runner.checkFlushRequest()
	if err != nil {
		t.Fatal(err)
	}

	err = runner.deferUnstable(integrity.UnstablePathsError{"/pending"})
	if err != nil {
		t.Fatal(err)
	}

	err = runner.completeFlushRequest()
	if err != nil || !runner.forceCommit {
		t.Fatal("incomplete flush reported complete", err)
	}

	_, err = os.Stat(filepath.Join(runner.cfg.Runtime, runner.name+flushRequestSuffix))
	if err != nil {
		t.Fatal("pending request disappeared", err)
	}

	clear(runner.unstable)

	err = runner.completeFlushRequest()
	if err != nil || runner.forceCommit {
		t.Fatal("completed flush remained pending", err)
	}
}
