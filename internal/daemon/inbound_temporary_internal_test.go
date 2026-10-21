package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/inbound"
)

func TestIncomingConflictRemovesInterruptedTemporary(t *testing.T) {
	t.Parallel()
	runner, remote, editor := incomingWorker(t)
	target := incomingEdit(t, runner, editor, "a", "remote bytes")
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

	runner.incoming, err = inbound.Prepare(
		t.Context(), runner.repo, runner.roots, runner.matcher, base, target, policy, differences,
	)
	if err != nil {
		t.Fatal(err)
	}

	// A crash interrupted the replacement, then imports were disabled before recovery.
	temporary := writeSource(t, runner, ".fsledger-inbound-"+runner.incoming.ID+"-0", "remote bytes")
	runner.cfg.Storage.Git.Bidirectional = false

	err = runner.resumeIncoming(t.Context())
	if err != nil || runner.incoming.Conflict == "" {
		t.Fatal("pending import not abandoned", err, runner.status)
	}

	_, err = os.Lstat(temporary)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("abandoned import left remote content in the source tree", err)
	}

	assertIncomingFile(t, filepath.Join(runner.roots[0], "a"), "initial")
}
