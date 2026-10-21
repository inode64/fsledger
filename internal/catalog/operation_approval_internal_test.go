package catalog

import (
	"testing"

	"github.com/inode64/fsledger/internal/integrity"
)

// acceptOperation approves an operation ID and requires it to resolve every pending difference.
func acceptOperation(t *testing.T, store *Store, identifier string) {
	t.Helper()

	err := store.Accept(t.Context(), identifier)
	if err != nil {
		t.Fatal(err)
	}

	if ids := pendingIDs(t, store); len(ids) != 0 {
		t.Fatalf("operation %s left differences pending: %v", identifier, ids)
	}

	if got := store.state.Stats.Violations; got != 0 {
		t.Fatalf("violations = %d, want 0", got)
	}
}

func approvedMode(t *testing.T, store *Store, record integrity.Record) uint32 {
	t.Helper()

	approved, _, err := readRecord(store.database, baselinePrefix(store.state.Baseline), record.Path)
	if err != nil {
		t.Fatal(err)
	}

	return approved.Mode
}

func TestOperationApprovalCoversRepeatedPath(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	first, second := workloadRecord(1), workloadRecord(2)
	reference(t, store, first, second)
	first.Mode, second.Mode = 0o644, 0o644
	observeRecords(t, store, "operation", first, second)
	// A file event followed by the reconciliation of its parent subtree observes the path again.
	first.Mode = 0o640
	observeRecords(t, store, "operation", first)

	acceptOperation(t, store, "operation")

	if approvedMode(t, store, first) != 0o640 || approvedMode(t, store, second) != 0o644 {
		t.Fatal("operation approval missed the current version of a recorded path")
	}
}

func TestOperationApprovalSurvivesResolvedPath(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	first, second, third := workloadRecord(1), workloadRecord(2), workloadRecord(3)
	reference(t, store, first, second, third)
	restored := first
	first.Mode, second.Mode, third.Mode = 0o644, 0o644, 0o644
	observeRecords(t, store, "operation", first, second)
	// The operation resolves one recorded path and only then records another one.
	observeRecords(t, store, "operation", restored, third)

	acceptOperation(t, store, "operation")

	if approvedMode(t, store, first) != restored.Mode ||
		approvedMode(t, store, second) != 0o644 || approvedMode(t, store, third) != 0o644 {
		t.Fatal("operation approval missed a recorded path or changed a resolved one")
	}
}
