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

func TestDeferredHashCannotApproveChangedMetadata(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	store.Policy.Hash.OnEvent = false
	store.Policy.Compare = append(store.Policy.Compare, fieldSize)
	original := workloadRecord(0)
	reference(t, store, original)
	metadata := original
	metadata.Size++
	metadata.Hash, metadata.Algorithm = "", ""

	prepared, _, err := store.prepare(t.Context(), integrity.NewScanner(1, 1), metadata, false)
	if err != nil {
		t.Fatal(err)
	}

	if prepared.Hash != "" || prepared.HashedAt != 0 {
		t.Fatal("new metadata retained an old digest")
	}

	observeRecords(t, store, "deferred-hash", prepared)

	err = store.Accept(t.Context(), "deferred-hash")
	if err == nil {
		t.Fatal("approved content that has not been hashed")
	}

	prepared.Hash, prepared.Algorithm = strings.Repeat("f", 64), original.Algorithm
	observeRecords(t, store, "verified-hash", prepared)

	err = store.Accept(t.Context(), "verified-hash")
	if err != nil {
		t.Fatal(err)
	}

	observeRecords(t, store, "repeated-verification", prepared)

	if len(pendingIDs(t, store)) != 0 {
		t.Fatal("verified approval raised another violation")
	}
}
