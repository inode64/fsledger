package catalog

import (
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/integrity"
)

// deferredObservation applies an event observation whose new metadata leaves its content unhashed.
func deferredObservation(t *testing.T, store *Store, identifier string, record integrity.Record) {
	t.Helper()

	record.Size++
	record.Mtime++
	record.Hash, record.Algorithm = "", ""

	prepared, previous, err := store.prepare(t.Context(), integrity.NewScanner(1, 1), record, false)
	if err != nil {
		t.Fatal(err)
	}

	if prepared.Hash != "" {
		t.Fatal("changed metadata retained an old digest")
	}

	update := mutation{refreshHash: false, excluded: false, record: prepared, previous: previous, deleted: false}

	_, _, err = store.apply(t.Context(), []mutation{update}, 0, identifier, "test actor")
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeferredHashKeepsPendingContentViolation(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	store.Policy.Hash.OnEvent = false
	tampered, untouched := workloadRecord(0), workloadRecord(1)
	reference(t, store, tampered, untouched)
	tampered.Hash = strings.Repeat("e", 64)
	observeRecords(t, store, "full-scan", tampered)
	original := pendingIDs(t, store)[string(tampered.Path)]

	deferredObservation(t, store, "tampered-event", tampered)
	deferredObservation(t, store, "untouched-event", untouched)

	// Unknown content neither resolves the recorded difference nor raises one for the verified file.
	ids := pendingIDs(t, store)
	if len(ids) != 1 || original == "" || ids[string(tampered.Path)] != original {
		t.Fatalf("deferred hash changed pending differences: before=%q after=%v", original, ids)
	}

	if got := store.state.Stats.Violations; got != 1 {
		t.Fatalf("violations = %d, want 1", got)
	}
}
