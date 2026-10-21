package daemon

import (
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/inbound"
)

// conflictedIncoming leaves the durable plan of a remote edit that a racing local write turned into a conflict.
func conflictedIncoming(t *testing.T) (*worker, string, string) {
	t.Helper()
	runner, remote, editor := incomingWorker(t)
	base := runner.status.LastCommit
	target := incomingEdit(t, runner, editor, "a", "remote bytes")

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

	runner.incoming = plan

	writeSource(t, runner, "a", "local bytes")

	err = runner.reconcile(t.Context(), "Local during incoming")
	if err != nil || runner.incoming == nil || runner.incoming.Conflict == "" || runner.status.SyncConflict == "" {
		t.Fatal("fixture did not record an incoming conflict", err, runner.status)
	}

	return runner, remote, editor
}

func TestIncomingResolvedConflictIsCleared(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		resolve func(t *testing.T, runner *worker, remote, editor string)
		name    string
	}{
		{name: "remote rewound behind local", resolve: func(t *testing.T, runner *worker, _, editor string) {
			t.Helper()
			incomingGit(t, editor, "push", "--force", "origin", "HEAD~1:refs/heads/"+runner.cfg.Storage.Git.Branch)
		}},
		{name: "local history pushed by hand", resolve: func(t *testing.T, runner *worker, remote, _ string) {
			t.Helper()
			incomingGit(t, runner.repo.Path, "push", "--force", remote, "HEAD:refs/heads/"+runner.cfg.Storage.Git.Branch)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			runner, remote, editor := conflictedIncoming(t)
			fetched := time.Now().Add(time.Hour)

			test.resolve(t, runner, remote, editor)
			runner.syncRemote(t.Context(), fetched)

			pending, err := runner.catalog.Incoming(t.Context())
			if err != nil || pending != nil || runner.incoming != nil || runner.status.SyncConflict != "" {
				t.Fatal("resolved conflict kept its incoming plan", pending != nil, runner.incoming != nil, err,
					runner.status.SyncConflict)
			}

			writeSource(t, runner, "b", "local after resolution")

			err = runner.reconcile(t.Context(), "Local after resolution")
			if err != nil || runner.status.SyncConflict != "" {
				t.Fatal("reconciliation restored a resolved conflict", err, runner.status)
			}

			// Publication between fetches is gated only by the current synchronization state.
			runner.syncRemote(t.Context(), fetched.Add(runner.cfg.Storage.Git.FetchInterval/2))

			if got := incomingGit(
				t,
				remote,
				"rev-parse",
				"refs/heads/"+runner.cfg.Storage.Git.Branch,
			); got != runner.status.LastCommit {
				t.Fatalf("publication did not resume: %s, %+v", got, runner.status)
			}
		})
	}
}
