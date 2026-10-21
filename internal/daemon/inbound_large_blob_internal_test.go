package daemon

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitrepo "github.com/inode64/fsledger/internal/git"
)

func TestIncomingOversizedBlobIsConflict(t *testing.T) {
	t.Parallel()
	runner, _, editor := incomingWorker(t)
	target := incomingEdit(t, runner, editor, "a", strings.Repeat("a", gitrepo.MaxInboundBlob+1))
	runner.syncRemote(t.Context(), time.Now())

	if !strings.Contains(runner.status.SyncConflict, "64 MiB") || runner.status.LastFetchError != "" ||
		runner.status.InboundApplied == target || runner.incomingActive() {
		t.Fatal("oversized incoming blob not exposed as conflict", runner.status)
	}

	pending, err := runner.catalog.Incoming(t.Context())
	if err != nil || pending == nil || pending.Target != target || !strings.Contains(pending.Conflict, "64 MiB") {
		t.Fatal("oversized incoming blob conflict not persisted for its target", pending, err)
	}

	assertIncomingFile(t, filepath.Join(runner.roots[0], "a"), "initial")
}
