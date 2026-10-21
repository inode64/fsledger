package daemon

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIncomingRewrittenRemoteIsDivergence(t *testing.T) {
	t.Parallel()
	runner, _, editor := incomingWorker(t)
	local := runner.status.LastCommit
	runner.syncRemote(t.Context(), time.Now())

	if runner.status.SyncConflict != "" || runner.status.LastFetchError != "" {
		t.Fatal("initial fetch did not match published history", runner.status)
	}

	// Squash the remote edit into the published commit, replacing the history the daemon fetched.
	incomingEdit(t, runner, editor, "a", "rewritten bytes")
	incomingGit(t, editor, "reset", "--soft", "HEAD~1")
	incomingGit(t, editor, "commit", "--amend", "-m", "Rewritten history")
	incomingGit(t, editor, "push", "--force", "origin", "HEAD:refs/heads/"+runner.cfg.Storage.Git.Branch)
	runner.syncRemote(t.Context(), time.Now().Add(time.Hour))

	if runner.status.LastFetchError != "" || runner.status.SyncConflict == "" || runner.incoming != nil {
		t.Fatal("rewritten remote not exposed as divergence", runner.status)
	}

	assertIncomingFile(t, filepath.Join(runner.roots[0], "a"), "initial")

	if got := strings.TrimSpace(gitOutput(t, runner, "rev-parse", "HEAD")); got != local {
		t.Fatalf("HEAD %s, want local %s", got, local)
	}
}

func TestIncomingRewoundRemoteResumesPublication(t *testing.T) {
	t.Parallel()
	runner, remote, editor := incomingWorker(t)
	incomingEdit(t, runner, editor, "a", "remote bytes")
	writeSource(t, runner, "b", "local bytes")

	err := runner.reconcile(t.Context(), "Local before divergence")
	if err != nil {
		t.Fatal(err)
	}

	runner.syncRemote(t.Context(), time.Now())

	if runner.status.SyncConflict == "" {
		t.Fatal("divergence not exposed", runner.status)
	}

	// Manual resolution drops the remote commit, leaving the published base on the branch.
	incomingGit(t, editor, "push", "--force", "origin", "HEAD~1:refs/heads/"+runner.cfg.Storage.Git.Branch)
	runner.syncRemote(t.Context(), time.Now().Add(time.Hour))

	if runner.status.LastFetchError != "" || runner.status.SyncConflict != "" {
		t.Fatal("rewound remote kept the conflict", runner.status)
	}

	if got := incomingGit(
		t,
		remote,
		"rev-parse",
		"refs/heads/"+runner.cfg.Storage.Git.Branch,
	); got != runner.status.LastCommit {
		t.Fatalf("publication did not resume: %s, %+v", got, runner.status)
	}

	assertIncomingFile(t, filepath.Join(runner.roots[0], "a"), "initial")
}
