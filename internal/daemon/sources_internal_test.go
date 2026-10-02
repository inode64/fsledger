package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/watcher"
)

func TestInaccessibleSourceIsRetainedNotDropped(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	writeSource(t, runner, "file", "content")

	err := runner.reconcile(t.Context(), "initial")
	if err != nil {
		t.Fatal(err)
	}

	root := runner.roots[0]
	// Without search permission on the parent the source cannot be told apart from a lost filesystem.
	blockSource(t, filepath.Dir(root))

	if runner.refreshSources() || runner.nextRoots != nil {
		t.Fatal("inaccessible source scheduled for removal")
	}

	if !slices.Equal(runner.status.UnavailableSources, []string{root}) {
		t.Fatal("retained source not reported:", runner.status.UnavailableSources)
	}
}

func TestFailedInitialGitCommitStillPinsDeletionClassification(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	writeSource(t, runner, "file", "content")

	hook := filepath.Join(runner.repo.Path, ".git", "hooks", "pre-commit")

	//nolint:gosec // A private executable Git hook simulates a failed commit.
	err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = runner.reconcile(t.Context(), "initial")
	if err == nil {
		t.Fatal("test did not exercise Git failure")
	}

	root := runner.roots[0]
	blockSource(t, filepath.Dir(root))

	if runner.refreshSources() || !slices.Equal(runner.status.UnavailableSources, []string{root}) {
		t.Fatal("failed initial Git commit left source loss classified as deletion", runner.status)
	}
}

func TestDirectoryRootCanBeReplacedByFile(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	writeSource(t, runner, "child", "inside directory")

	err := runner.reconcile(t.Context(), "initial")
	if err != nil {
		t.Fatal(err)
	}

	root := runner.roots[0]

	err = os.RemoveAll(root)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(root, []byte("now a file"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = runner.reconcile(t.Context(), "root replacement")
	if err != nil {
		t.Fatal("root replacement blocked repository", err)
	}

	content := gitOutput(t, runner, "show", "HEAD:"+strings.TrimPrefix(root, "/"))
	if content != "now a file" {
		t.Fatal("root replacement was not committed", content)
	}
}

func TestReturnedUnavailableSourceForcesRebind(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	root := runner.roots[0]
	runner.status.UnavailableSources = []string{root}

	if !runner.refreshSources() || !runner.sourcesPending || !slices.Equal(runner.nextRoots, runner.roots) {
		t.Fatal("returned source did not schedule watcher and identity refresh")
	}
}

func TestAdoptedSourcesKeepDeliveringEventsAfterTheOperationEnds(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	runner.cfg.Watch.Backend = event.Inotify
	runner.queue = watcher.NewQueue(8)

	added := filepath.Join(filepath.Dir(runner.roots[0]), "added")

	err := os.Mkdir(added, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	// Sources change inside one bounded operation, whose context ends when the flush returns.
	operation, finish := context.WithCancel(t.Context())
	runner.nextRoots, runner.sourcesPending = []string{added, runner.roots[0]}, true
	runner.applySources(operation)

	defer runner.stopWatching()

	finish()

	err = os.WriteFile(filepath.Join(added, "file"), []byte("content"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-runner.queue.Channel:
	case <-time.After(5 * time.Second):
		t.Fatal("watchers of adopted sources stopped with the operation that created them")
	}
}
