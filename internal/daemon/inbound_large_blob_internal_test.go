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
		runner.status.InboundApplied == target || runner.incoming != nil {
		t.Fatal("oversized incoming blob not exposed as conflict", runner.status)
	}

	assertIncomingFile(t, filepath.Join(runner.roots[0], "a"), "initial")
}
