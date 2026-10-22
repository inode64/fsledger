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
	observeInitial(t, store, original)
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

	changed, err := store.apply(t.Context(), updates, 1, "repeated-path", "test")
	if err != nil || changed != 2 {
		t.Fatalf("batch must observe modification and reversion: changed=%d err=%v", changed, err)
	}

	current, exists, err := store.Current(t.Context(), original.Path)
	if err != nil || !exists || current.Hash != original.Hash {
		t.Fatalf("reversion not kept as the current observation: %+v %v", current, err)
	}
}

// Without hashing on events, new metadata leaves the content unverified: the old digest is dropped,
// the unknown content is no change, and the next verified digest is.
func TestDeferredHashDropsDigestOfNewMetadata(t *testing.T) {
	t.Parallel()
	cfg := testStoreConfig(t)
	cfg.Integrity.Hash.OnEvent = false
	store := openTestStore(t, t.TempDir(), cfg.Integrity, cfg.Notifications)
	original := workloadRecord(0)
	observeInitial(t, store, original)
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

	apply := func(identifier string, record integrity.Record) int {
		t.Helper()

		update := mutation{record: record, previous: nil, excluded: false, deleted: false, refreshHash: false}

		changed, applyErr := store.apply(t.Context(), []mutation{update}, 1, identifier, "test")
		if applyErr != nil {
			t.Fatal(applyErr)
		}

		return changed
	}

	if changed := apply("deferred-hash", prepared); changed != 0 {
		t.Fatal("unverified content recorded as a change")
	}

	prepared.Hash, prepared.Algorithm = strings.Repeat("f", 64), original.Algorithm
	if changed := apply("verified-hash", prepared); changed != 1 {
		t.Fatal("verified new content not recorded as a change")
	}
}
