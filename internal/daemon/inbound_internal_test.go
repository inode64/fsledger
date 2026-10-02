package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/filter"
	"github.com/inode64/fsledger/internal/inbound"
)

func incomingGit(t *testing.T, path string, args ...string) string {
	t.Helper()
	//nolint:gosec // Arguments target only private temporary repositories owned by this test.
	output, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", path}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}

	return strings.TrimSpace(string(output))
}

func incomingWorker(t *testing.T) (*worker, string, string) {
	t.Helper()
	runner := makeWorker(t)
	writeSource(t, runner, "a", "initial")
	writeSource(t, runner, "b", "initial")

	err := runner.reconcile(t.Context(), "Initial snapshot")
	if err != nil {
		t.Fatal(err)
	}

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

	return runner, remote, editor
}

func incomingEdit(t *testing.T, runner *worker, editor, name, value string) string {
	t.Helper()

	path := filepath.Join(editor, strings.TrimPrefix(runner.roots[0], "/"), name)

	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(path, []byte(value), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	incomingGit(t, editor, "add", "--all")
	incomingGit(t, editor, "commit", "-m", "Remote change")
	incomingGit(t, editor, "push", "origin", "HEAD:refs/heads/"+runner.cfg.Storage.Git.Branch)

	return incomingGit(t, editor, "rev-parse", "HEAD")
}

func assertIncomingFile(t *testing.T, path, expected string) {
	t.Helper()
	//nolint:gosec // The test owns this temporary source path.
	data, err := os.ReadFile(path)
	if err != nil || string(data) != expected {
		t.Fatalf("source %q: got %q, want %q, error %v", path, data, expected, err)
	}
}

func TestIncomingRoundTrip(t *testing.T) {
	t.Parallel()
	runner, remote, editor := incomingWorker(t)

	err := runner.catalog.InitBaseline(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	target := incomingEdit(t, runner, editor, "a", "remote bytes")
	runner.syncRemote(t.Context(), time.Now())

	if runner.status.InboundApplied != target {
		t.Fatalf("incoming failed: %+v", runner.status)
	}

	assertIncomingFile(t, filepath.Join(runner.roots[0], "a"), "remote bytes")

	stats, err := runner.catalog.Stats(t.Context())
	if err != nil || stats.Violations == 0 {
		t.Fatal("incoming change silently approved integrity baseline", stats, err)
	}

	if got := strings.TrimSpace(gitOutput(t, runner, "rev-parse", "HEAD")); got != target {
		t.Fatalf("HEAD %s, want remote %s", got, target)
	}

	writeSource(t, runner, "b", "local after remote")

	err = runner.reconcile(t.Context(), "Local after import")
	if err != nil {
		t.Fatal(err)
	}

	runner.syncRemote(t.Context(), time.Now().Add(time.Hour))

	if got := incomingGit(
		t,
		remote,
		"rev-parse",
		"refs/heads/"+runner.cfg.Storage.Git.Branch,
	); got != runner.status.LastCommit {
		t.Fatalf("publication did not resume: %s, %+v", got, runner.status)
	}
}

func TestIncomingWaitsForDeferredVersions(t *testing.T) {
	t.Parallel()
	runner, _, editor := incomingWorker(t)
	runner.cfg.Commit.Defer = []filter.Rule{{Paths: []string{"**/b"}, Users: nil, CommandRegex: nil}}

	err := runner.loadDeferral(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	writeSource(t, runner, "b", "deferred local version")

	err = runner.reconcile(t.Context(), "Deferred local edit")
	if err != nil || len(runner.deferred) != 1 {
		t.Fatal("fixture did not defer local bytes", err)
	}

	incomingEdit(t, runner, editor, "a", "remote bytes")
	runner.syncRemote(t.Context(), time.Now())

	if runner.incoming != nil || runner.status.SyncBlocked == "" {
		t.Fatal("incoming ignored pending deferred version", runner.status)
	}

	assertIncomingFile(t, filepath.Join(runner.roots[0], "a"), "initial")
	assertIncomingFile(t, filepath.Join(runner.roots[0], "b"), "deferred local version")
}

func TestIncomingNetworkFailurePreservesLocalAttribution(t *testing.T) {
	t.Parallel()
	runner, _, _ := incomingWorker(t)
	runner.cfg.Storage.Git.Remote = filepath.Join(t.TempDir(), "missing-remote")
	runner.syncRemote(t.Context(), time.Now())

	if runner.status.LastFetchError == "" || runner.dirty || runner.contaminated || runner.pendingError {
		t.Fatal("network failure contaminated local worker", runner.status)
	}

	writeSource(t, runner, "a", "local without remote")

	err := runner.reconcile(t.Context(), "Local while offline")
	if err != nil {
		t.Fatal(err)
	}
}

func TestIncomingDivergencePreservesLocal(t *testing.T) {
	t.Parallel()
	runner, _, editor := incomingWorker(t)
	incomingEdit(t, runner, editor, "a", "remote bytes")
	writeSource(t, runner, "a", "unobserved local bytes")
	runner.syncRemote(t.Context(), time.Now())
	assertIncomingFile(t, filepath.Join(runner.roots[0], "a"), "unobserved local bytes")
	runner.syncRemote(t.Context(), time.Now().Add(time.Hour))

	if runner.status.SyncConflict == "" {
		t.Fatal("divergence not exposed", runner.status)
	}

	writeSource(t, runner, "b", "local still works")

	err := runner.reconcile(t.Context(), "Local during conflict")
	if err != nil {
		t.Fatal("remote conflict blocked local history", err)
	}
}

//nolint:funlen // Ordered crash simulation verifies durable progress and recovery without extra commits.
func TestIncomingJournalRecovery(t *testing.T) {
	t.Parallel()
	runner, remote, editor := incomingWorker(t)
	incomingEdit(t, runner, editor, "a", "first incoming")
	target := incomingEdit(t, runner, editor, "b", "second incoming")
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

	plan, err := inbound.Prepare(
		t.Context(),
		runner.repo,
		runner.roots,
		runner.matcher,
		base,
		target,
		policy,
		differences,
	)
	if err != nil {
		t.Fatal(err)
	}

	err = runner.catalog.SaveIncoming(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}

	interrupted := fault.New("simulated interruption before progress save")

	err = plan.Apply(t.Context(), runner.repo, func() error { return interrupted })
	if !errors.Is(err, interrupted) {
		t.Fatal(err)
	}

	// Reload the on-disk journal, whose first replacement has not been marked done.
	runner.incoming, err = runner.catalog.Incoming(t.Context())
	if err != nil || runner.incoming == nil || runner.incoming.Done != 0 {
		t.Fatal("invalid persisted journal", err)
	}

	err = runner.reconcile(t.Context(), "Startup recovery")
	if err != nil {
		t.Fatal(err)
	}

	assertIncomingFile(t, filepath.Join(runner.roots[0], "a"), "first incoming")
	assertIncomingFile(t, filepath.Join(runner.roots[0], "b"), "second incoming")

	if got := strings.TrimSpace(gitOutput(t, runner, "rev-parse", "HEAD")); got != target {
		t.Fatal("recovery created an extra local commit", got, target)
	}

	pending, err := runner.catalog.Incoming(t.Context())
	if err != nil || pending != nil {
		t.Fatal("recovery retained active journal", pending, err)
	}
}

func TestIncomingOutsideSelectionAndDisabled(t *testing.T) {
	t.Parallel()
	runner, _, editor := incomingWorker(t)
	target := incomingEdit(t, runner, editor, "../../unselected", "must not appear")
	runner.syncRemote(t.Context(), time.Now())

	if runner.status.SyncBlocked == "" || runner.status.InboundApplied == target || runner.incoming != nil {
		t.Fatal("unsafe remote tree accepted", runner.status)
	}

	runner.cfg.Storage.Git.Bidirectional = false
	runner.status.SyncBlocked = ""
	runner.syncRemote(t.Context(), time.Now().Add(time.Hour))

	if runner.status.SyncBlocked != "" || runner.incoming != nil {
		t.Fatal("disabled incoming attempted an import")
	}
}
