package daemon

import (
	"context"
	"slices"
	"testing"

	"github.com/inode64/fsledger/internal/integrity"
)

// A forced copy that stays unstable must keep the hash marker. Clearing it would make the next
// reconciliation trust the unchanged signature and leave the old mirror bytes in place.
func TestUnstableHashRefreshKeepsItsMarker(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	runner.catalog.BindCopiedHashes(runner.mirror.CopiedHash)
	runner.cfg.Watch.Reconcile.Enabled = false

	path := writeSource(t, runner, "tracked", "original")

	err := runner.reconcile(t.Context(), "Initial snapshot")
	if err != nil {
		t.Fatal(err)
	}

	writeSource(t, runner, "tracked", "rewritten bytes")

	runner.refreshPath = func(context.Context, string) error {
		return integrity.UnstablePathsError{path}
	}

	t.Cleanup(func() { runner.refreshPath = nil })

	runner.fullScanRequested = true

	err = runner.reconcile(t.Context(), "hash changed")
	if err != nil || len(runner.unstable) != 1 {
		t.Fatal("unstable refresh became a failure", err, runner.unstable)
	}

	if pending := pendingRefreshes(t, runner); !slices.Contains(pending, path) {
		t.Fatal("unstable refresh dropped its marker", pending)
	}

	runner.refreshPath = nil

	err = runner.reconcile(t.Context(), "retry hash refresh")
	if err != nil {
		t.Fatal(err)
	}

	if pending := pendingRefreshes(t, runner); len(pending) != 0 {
		t.Fatal("completed refresh kept its marker", pending)
	}

	checkGitContent(t, runner, path, "rewritten bytes")
}
