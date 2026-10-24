package daemon

import (
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
)

func pendingRefreshes(t *testing.T, runner *worker) []string {
	t.Helper()

	var paths []string

	err := runner.catalog.ChangedHashes(t.Context(), func(path string) error {
		paths = append(paths, path)

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return paths
}

// importedWorker returns a worker whose catalog just recorded an incoming change to a regular file: the
// full scan of the import compares hashes as a full reconciliation does.
func importedWorker(t *testing.T) *worker {
	t.Helper()

	runner, _, editor := incomingWorker(t)
	incomingEdit(t, runner, editor, "a", "remote bytes")
	runner.syncRemote(t.Context(), time.Now())

	if runner.status.InboundApplied == "" {
		t.Fatal("incoming change not applied", runner.status)
	}

	return runner
}

// A pending mirror refresh belongs to its catalog entry: once the entry is gone, as when its source leaves
// the selection, the refresh must go too, or every later reconciliation fails on a path outside the sources.
func TestRefreshOfADeselectedSourceIsDropped(t *testing.T) {
	t.Parallel()

	runner := importedWorker(t)

	runner.updateSources(config.Sources{Roots: []string{t.TempDir()}, Missing: nil, Skipped: nil, Incomplete: false})
	runner.applySources(t.Context())

	defer runner.stopWatching()

	for range 2 {
		err := runner.reconcile(t.Context(), "after deselection")
		if err != nil {
			t.Fatal("a refresh outside the sources wedged reconciliation:", err)
		}
	}

	if pending := pendingRefreshes(t, runner); len(pending) != 0 {
		t.Fatal("refresh of a deleted entry kept:", pending)
	}
}
