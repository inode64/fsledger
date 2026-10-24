package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/inbound"
)

// journalIncoming publishes the worker's history to a new remote, adds path there with an editor and journals
// the plan of that incoming change, as one interrupted before it was applied.
func journalIncoming(t *testing.T, runner *worker, path string) {
	t.Helper()

	remote, editor := t.TempDir(), t.TempDir()
	incomingGit(t, remote, "init", "--bare")

	runner.cfg.Storage.Git.Bidirectional = true
	runner.cfg.Storage.Git.Remote = remote
	runner.publish(t.Context(), time.Now())

	if runner.status.LastPublishError != "" {
		t.Fatal(runner.status.LastPublishError)
	}

	incomingGit(t, editor, "clone", "--branch", runner.cfg.Storage.Git.Branch, remote, ".")
	incomingGit(t, editor, "config", "user.name", "Remote editor")
	incomingGit(t, editor, "config", "user.email", "editor@example.invalid")

	target := incomingEdit(t, runner, editor, path, "remote bytes")
	base := runner.status.LastCommit

	_, err := runner.repo.FetchBranch(t.Context(), remote, runner.cfg.Storage.Git.Branch)
	if err != nil {
		t.Fatal(err)
	}

	differences, err := runner.repo.TreeChanges(t.Context(), base, target)
	if err != nil {
		t.Fatal(err)
	}

	policy, err := runner.incomingPolicy()
	if err != nil {
		t.Fatal(err)
	}

	plan, err := inbound.Prepare(t.Context(), runner.repo, runner.roots, runner.matcher, base, target, policy,
		differences)
	if err == nil {
		err = errors.Join(runner.repo.PinIncoming(t.Context(), target), runner.catalog.SaveIncoming(t.Context(), plan))
	}

	if err != nil {
		t.Fatal(err)
	}

	runner.incoming = plan
}

// A journaled incoming change resumed while a recorded filesystem is missing would land on its empty mount
// point: it waits, then applies onto the filesystem once mounted again.
func TestKernelJournaledIncomingWaitsForAMissingFilesystem(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	nested, data := filepath.Join(runner.roots[0], "data"), t.TempDir()
	mountNested(t, data, nested)
	persistentMount(t, nested)
	writeSource(t, runner, "data/file", "nested")

	err := runner.reconcile(t.Context(), "mounted")
	if err != nil {
		t.Fatal(err)
	}

	journalIncoming(t, runner, "data/new")

	err = unix.Unmount(nested, 0)
	if err != nil {
		t.Fatal(err)
	}

	waited(t, runner, runner.reconcile(t.Context(), "unmounted"), nested)

	_, err = os.Lstat(filepath.Join(nested, "new"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the incoming change landed on the empty mount point", err)
	}

	mountNested(t, data, nested)

	err = runner.reconcile(t.Context(), "mounted again")
	if err != nil || runner.incomingActive() || runner.status.SyncConflict != "" {
		t.Fatal("the incoming change was not applied once mounted", err, runner.status.SyncConflict)
	}

	_, err = os.Lstat(filepath.Join(data, "new"))
	if err != nil {
		t.Fatal("the incoming change did not reach the filesystem", err)
	}
}
