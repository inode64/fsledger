package catalog

import (
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/integrity"
)

func TestPreparedObservationsRespectEarlierBatchWrites(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	original := workloadRecord(0)
	reference(t, store, original)
	metadata := original
	metadata.Hash, metadata.Algorithm = "", ""

	prepared, previous, err := store.prepare(t.Context(), integrity.NewScanner(1, 2), metadata, false)
	if err != nil || previous == nil || prepared.Hash != original.Hash {
		t.Fatalf("prepare existing observation: %v", err)
	}

	modified := prepared
	modified.Hash = strings.Repeat("f", 64)
	updates := []mutation{
		{refreshHash: false, excluded: false, record: modified, previous: previous, deleted: false},
		{refreshHash: false, excluded: false, record: prepared, previous: previous, deleted: false},
	}

	changed, _, err := store.apply(t.Context(), updates, 1, "repeated-path", "test")
	if err != nil || changed != 2 {
		t.Fatalf("batch must observe modification and reversion: changed=%d err=%v", changed, err)
	}

	if ids := pendingIDs(t, store); len(ids) != 0 {
		t.Fatalf("reversion left pending differences: %v", ids)
	}
}
