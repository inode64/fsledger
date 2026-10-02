package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/event"
	gitrepo "github.com/inode64/fsledger/internal/git"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/mirror"
	"github.com/inode64/fsledger/internal/resource"
	"github.com/inode64/fsledger/internal/watcher"
	"github.com/inode64/fsledger/internal/watcher/fanotify"
)

//nolint:funlen // Ordered integration fixture keeps setup, transitions and assertions together.
func makeWorker(t *testing.T) *worker {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")

	runtime := filepath.Join(root, "run")
	for _, path := range []string{source, runtime} {
		err := os.Mkdir(path, 0o700)
		if err != nil {
			t.Fatal(err)
		}
	}

	for name, text := range map[string]string{
		"main.yaml": fmt.Sprintf(`storage: {path: %s/repos}
runtime: %s
paths: {repositories: [system.yaml]}`, root, runtime),
		"system.yaml": fmt.Sprintf("paths: [%s]", source),
	} {
		err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := config.Load(filepath.Join(root, "main.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	repo := &gitrepo.Repository{
		Path:    filepath.Join(cfg.Storage.Path, "system"),
		Host:    "test",
		Timeout: cfg.Storage.Git.Timeout,
	}

	_, initErr := repo.Init(t.Context())
	if initErr != nil {
		t.Fatal(initErr)
	}

	matcher, err := cfg.Matcher("system")
	if err != nil {
		t.Fatal(err)
	}

	syncer, err := mirror.Open(repo.Path, []string{source}, matcher, "sha256")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.Close(syncer) })

	store, err := catalog.Open(
		t.Context(),
		cfg.CatalogPath("system"),
		"system",
		"test",
		cfg.Integrity,
		cfg.Notifications,
	)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.Close(store) })

	cfg.Commit.Debounce = time.Millisecond
	cfg.Commit.MaxDelay = time.Second

	return newWorker(
		cfg,
		"system",
		slog.New(slog.DiscardHandler),
		integrity.NewScanner(1, 2),
		store,
		nil,
		repo,
		syncer,
		matcher,
		newActiveRoots(cfg.Repositories["system"].Paths),
		sharedPIDFDProbe(fanotify.ProbePIDFDLifetime),
	)
}

func writeSource(t *testing.T, runner *worker, name, value string) string {
	t.Helper()

	path := filepath.Join(runner.status.Paths[0], name)

	err := os.WriteFile(path, []byte(value), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

func gitOutput(t *testing.T, runner *worker, args ...string) string {
	t.Helper()

	//nolint:gosec // This test uses only its private temporary directory and the current test process.
	out, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", runner.repo.Path}, args...)...).Output()
	if err != nil {
		t.Fatal(err)
	}

	return string(out)
}

func TestInterleavedActorsProduceIsolatedCommits(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	pathA := writeSource(t, runner, "a", "initial")

	pathB := writeSource(t, runner, "b", "initial")

	err := runner.reconcile(t.Context(), "Initial snapshot")
	if err != nil {
		t.Fatal(err)
	}

	writeSource(t, runner, "a", "actor A")
	writeSource(t, runner, "b", "actor B")

	now := time.Now()
	actorA := event.Actor{
		PID:        10,
		StartTime:  100,
		UID:        1000,
		LoginUID:   1000,
		UserKnown:  true,
		LoginKnown: true,
		Known:      true,
	}
	actorB := actorA
	actorB.PID = 11
	actorB.UID = 1001
	actorB.LoginUID = 1001

	runner.accept(event.Raw{Path: pathA, Actor: actorA, Backend: event.Fanotify, Time: now})
	runner.accept(event.Raw{Path: pathB, Actor: actorB, Backend: event.Fanotify, Time: now.Add(time.Millisecond)})
	runner.flush(t.Context(), now.Add(time.Second))

	if count := strings.TrimSpace(gitOutput(t, runner, "rev-list", "--count", "HEAD")); count != "3" {
		t.Fatalf("want initial+2 commits, got %s", count)
	}

	first := gitOutput(t, runner, "show", "HEAD^", "--format=", "--name-only")

	second := gitOutput(t, runner, "show", "HEAD", "--format=", "--name-only")
	if !strings.Contains(first, "/a") || strings.Contains(first, "/b") || !strings.Contains(second, "/b") ||
		strings.Contains(second, "/a") {
		t.Fatalf("actors mixed: first=%q second=%q", first, second)
	}
}

func TestOverflowReconcilesUnobservedFile(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	writeSource(t, runner, "a", "initial")

	reconcileErr := runner.reconcile(t.Context(), "Initial snapshot")
	if reconcileErr != nil {
		t.Fatal(reconcileErr)
	}

	path := writeSource(t, runner, "lost", "missing event")
	queue := watcher.NewQueue(1)
	runner.queue = queue
	queue.Send(event.Raw{Path: "/irrelevant"})
	queue.Send(event.Raw{Path: path})
	runner.flush(t.Context(), time.Now())

	relative, err := mirror.Relative(path)
	if err != nil {
		t.Fatal(err)
	}

	if got := gitOutput(t, runner, "show", "HEAD:"+relative); got != "missing event" {
		t.Fatalf("overflow lost content: %q", got)
	}

	if runner.dirty {
		t.Fatal("recovery stayed dirty")
	}
}

func TestFailedCommitRecoversWithoutMisattribution(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	writeSource(t, runner, "a", "initial")

	err := runner.reconcile(t.Context(), "Initial snapshot")
	if err != nil {
		t.Fatal(err)
	}

	hook := filepath.Join(runner.repo.Path, ".git", "hooks", "pre-commit")

	//nolint:gosec // This test uses only its private temporary directory and the current test process.
	err = os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	path := writeSource(t, runner, "a", "actor A")
	runner.accept(
		event.Raw{
			Path:    path,
			Backend: event.Fanotify,
			Actor:   event.Actor{PID: 1, StartTime: 1, UserKnown: true, Known: true},
		},
	)
	runner.flush(t.Context(), time.Now().Add(time.Second))

	if !runner.dirty || !runner.contaminated {
		t.Fatal("Git failure was not retained")
	}

	writeSource(t, runner, "b", "unattributed B")

	err = os.Remove(hook)
	if err != nil {
		t.Fatal(err)
	}

	runner.flush(t.Context(), time.Now().Add(2*time.Second))

	subject := gitOutput(t, runner, "log", "-1", "--format=%s")
	if !strings.HasPrefix(subject, "unknown:") {
		t.Fatalf("partial Git index attributed to an actor: %s", subject)
	}

	if count := strings.TrimSpace(gitOutput(t, runner, "rev-list", "--count", "HEAD")); count != "2" {
		t.Fatalf("unexpected recovery commits: %s", count)
	}
}

func TestWatcherReconciliationFlushDoesNotAnnounceFailure(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)

	queue := watcher.NewQueue(1)

	runner.watchers = []watcher.Watcher{&queuedDetector{Queue: queue}}
	for range 3 {
		queue.Send(event.Raw{Dirty: true, Reason: event.Reconciliation, Backend: event.Fanotify})
	}

	runner.checkLosses()

	if !runner.dirty || runner.contaminated || queue.ReconciliationPending() {
		t.Fatal("watcher scan requests were not consumed as ordinary reconciliation")
	}

	runner.flush(t.Context(), time.Now())

	if runner.pendingError || len(runner.status.Warnings) != 0 || runner.dirty {
		t.Fatal("routine polling reported failure", runner.status.Warnings)
	}
}

// blockSource makes reconciliation fail without removing the source: a deleted source is a
// recorded deletion, whereas an unreadable one must keep the mirror and back off.
func blockSource(t *testing.T, root string) {
	t.Helper()

	if os.Geteuid() == 0 {
		t.Skip("root reads directories regardless of their mode")
	}

	err := os.Chmod(root, 0)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		//nolint:gosec // Restores the private temporary directory so the test framework can remove it.
		restoreErr := os.Chmod(root, 0o700)
		if restoreErr != nil {
			t.Error(restoreErr)
		}
	})
}

func TestReconcileFailureBacksOffAndKeepsMirror(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	path := writeSource(t, runner, "file", "original")

	err := runner.reconcile(t.Context(), "initial")
	if err != nil {
		t.Fatal(err)
	}

	blockSource(t, runner.status.Paths[0])

	runner.dirty = true
	queue := watcher.NewQueue(1)
	runner.queue = queue
	runner.flush(t.Context(), time.Now())
	retry := runner.retryAt
	warnings := len(runner.status.Warnings)
	runner.flush(t.Context(), time.Now())

	if !runner.retryAt.Equal(retry) || len(runner.status.Warnings) != warnings {
		t.Fatal("retry spun at scheduling frequency")
	}

	if runner.retryDelay != time.Second {
		t.Fatal("missing initial retry delay", runner.retryDelay)
	}

	relative, err := mirror.Relative(path)
	if err != nil {
		t.Fatal(err)
	}

	if got := gitOutput(t, runner, "show", "HEAD:"+relative); got != "original" {
		t.Fatal("missing root deleted history", got)
	}

	runner.flush(t.Context(), retry.Add(time.Second))

	if runner.retryDelay != 2*time.Second {
		t.Fatal("retry did not back off", runner.retryDelay)
	}
}

func TestExplicitReconciliationDeadlineIsRespected(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	writeSource(t, runner, "file", "pending")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := runner.reconcile(ctx, "shutdown")
	if !errors.Is(err, context.Canceled) {
		t.Fatal("reconciliation discarded deadline", err)
	}
}

func TestStatusUsesCachedGitQuery(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	runner.saveStatus(t.Context())
	updated := runner.status.GitStatusUpdated
	previous := runner.status.GitStatus
	runner.repo.Path = filepath.Join(t.TempDir(), "not-a-repository")
	runner.saveStatus(t.Context())

	if !runner.status.GitStatusUpdated.Equal(updated) || runner.status.GitStatus != previous {
		t.Fatal("status queried Git again within its cache interval")
	}
}

func TestInitialReconcileFailureDoesNotStopWorker(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	runner.cfg.Watch.Backend = event.Polling
	blockSource(t, runner.status.Paths[0])

	var err error

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	finished := make(chan error, 1)
	go func() { finished <- runner.run(ctx, true) }()

	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()

	select {
	case err = <-finished:
		t.Fatal("initial failed snapshot stopped worker", err)
	case <-timer.C:
	}

	cancel()

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not honor shutdown")
	}

	if len(runner.status.Warnings) == 0 || runner.retryDelay == 0 {
		t.Fatal("test did not exercise failed initial reconciliation")
	}
}

func TestPublicationFailureDoesNotContaminateLocalCommits(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	writeSource(t, runner, "config", "initial")

	err := runner.reconcile(t.Context(), "initial")
	if err != nil {
		t.Fatal(err)
	}

	runner.cfg.Storage.Git.Remote = filepath.Join(t.TempDir(), "offline.git")
	runner.cfg.Storage.Git.Branch = "web01"
	runner.publish(t.Context(), time.Now())

	if runner.status.LastPublishError == "" || runner.contaminated || runner.dirty || runner.pendingError {
		t.Fatal("network outage contaminated local filesystem state", runner.status)
	}

	next := runner.publishNext
	runner.publish(t.Context(), time.Now())

	if !runner.publishNext.Equal(next) {
		t.Fatal("publication retried before backoff elapsed")
	}

	writeSource(t, runner, "config", "while offline")
	previous := runner.status.LastCommit

	err = runner.reconcile(t.Context(), "offline change")
	if err != nil || runner.status.LastCommit == previous {
		t.Fatal("local history stopped during remote outage", err)
	}
}

func TestAutomaticFanotifyFallbackIsNotEventLoss(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("requires an unprivileged process to exercise unavailable fanotify")
	}

	runner := makeWorker(t)
	runner.cfg.Watch.Backend = config.BackendAuto

	label := runner.tryFanotify(t.Context(), runner.cfg.Repositories[runner.name].Paths[0], runner.matcher)
	if label != "" {
		t.Fatal("unprivileged fixture unexpectedly acquired fanotify")
	}

	if runner.pendingError || len(runner.status.Warnings) != 0 {
		t.Fatal("ordinary automatic fallback reported event loss")
	}

	runner.cfg.Watch.Backend = event.Fanotify
	runner.tryFanotify(t.Context(), runner.cfg.Repositories[runner.name].Paths[0], runner.matcher)

	if !runner.pendingError {
		t.Fatal("explicit unavailable backend did not warn")
	}
}

func TestReconciliationSharesChangeIDWithCommit(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	writeSource(t, runner, "a", "initial")

	err := runner.reconcile(t.Context(), "Initial snapshot")
	if err != nil {
		t.Fatal(err)
	}

	body := gitOutput(t, runner, "log", "-1", "--format=%B")
	_, got, found := strings.Cut(body, gitrepo.ChangeIDTrailer)
	got, _, _ = strings.Cut(strings.TrimSpace(got), "\n")

	if !found || got != runner.status.LastChange {
		t.Fatalf("commit trailer %q differs from catalog change %q", got, runner.status.LastChange)
	}
}

func TestShutdownRepeatsNoScanThatStopInterrupted(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		interrupted bool
		polling     bool
		scans       bool
	}{
		{name: "completed", interrupted: false, polling: false, scans: true},
		{name: "interrupted", interrupted: true, polling: false, scans: false},
		// Without a startup reconciliation nothing would resume the interrupted work.
		{name: "polling without startup scan", interrupted: true, polling: true, scans: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			runner := makeWorker(t)
			writeSource(t, runner, "file", "pending")

			if test.polling {
				runner.cfg.Watch.Backend = event.Polling
				runner.cfg.Watch.Reconcile.OnStart = false
			}

			operation, cancel := context.WithCancel(t.Context())
			cancel()

			if !runner.interruptedByStop(operation, context.Canceled) {
				t.Fatal("cancelled operation not recognized")
			}

			runner.interrupted = test.interrupted

			err := runner.shutdown(operation)
			if err != nil || runner.status.Running {
				t.Fatal(err)
			}

			if scanned := !runner.status.Scan.Started.IsZero(); scanned != test.scans {
				t.Fatalf("shutdown reconciliation ran=%v, want %v", scanned, test.scans)
			}
		})
	}
}

func TestInterruptionIsNotReportedAsFailure(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	operation, cancel := context.WithCancel(t.Context())

	if runner.interruptedByStop(operation, context.Canceled) {
		t.Fatal("live operation treated as interrupted")
	}

	cancel()

	if runner.interruptedByStop(operation, os.ErrPermission) {
		t.Fatal("real failure treated as interruption")
	}

	err := runner.reconcile(operation, "startup")
	if !runner.interruptedByStop(operation, err) || !runner.interrupted {
		t.Fatal("cancelled reconciliation not recorded as interrupted", err)
	}

	if len(runner.status.Warnings) != 0 || runner.pendingError || runner.retryDelay != 0 ||
		runner.status.Scan.Result != "interrupted" {
		t.Fatal("interruption reported as failure", runner.status.Warnings, runner.status.Scan.Result)
	}
}

func TestShutdownGraceStartsAtStopRequest(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)

	if remaining := time.Until(runner.stopDeadline()); remaining < shutdownGrace-time.Second {
		t.Fatal("grace shortened before any stop request", remaining)
	}

	runner.stopRequested.Store(time.Now().Add(-gitOperationTimeout).UnixNano())

	if remaining := time.Until(runner.stopDeadline()); remaining > shutdownGrace-gitOperationTimeout {
		t.Fatal("operation grace added to shutdown grace", remaining)
	}
}
