package catalog

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/resource"
)

func TestNestedDirectoriesScanOnce(t *testing.T) {
	t.Parallel()
	store, scanner, matcher, root := subtreeFixture(t)
	generation := store.state.Generation
	paths := []string{filepath.Join(root, "a"), root, filepath.Join(root, "ab"), root}

	_, err := store.Observe(t.Context(), scanner, paths, []string{root}, matcher, "actor", "nested")
	if err != nil {
		t.Fatal(err)
	}

	if store.state.Generation != generation+1 {
		t.Fatal("redundant directory scans", store.state.Generation-generation)
	}
}

func TestUnchangedEventPreservesScanMarker(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	record := workloadRecord(0)
	reference(t, store, record)
	record.Observed++

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	changed, violation, err := store.applyOne(
		t.Context(),
		transaction,
		mutation{refreshHash: false, excluded: false, record: record, previous: nil, deleted: false},
		0,
		"same",
		"actor",
	)
	if err != nil || changed || violation {
		t.Fatal(changed, violation, err)
	}

	if transaction.batch.Count() != 0 {
		t.Fatal("unchanged event wrote file state")
	}
}

func TestEventMarkerSurvivesUntilNextScan(t *testing.T) {
	t.Parallel()
	store, scanner, matcher, root := subtreeFixture(t)
	path := []byte(filepath.Join(root, "a", "kept"))

	before, closer, err := store.database.Get(key(seenPrefix, path))
	if err != nil {
		t.Fatal(err)
	}

	saved := bytes.Clone(before)

	resource.Close(closer)

	_, err = store.Observe(t.Context(), scanner, []string{string(path)}, []string{root}, matcher, "actor", "direct")
	if err != nil {
		t.Fatal(err)
	}

	after, closer, err := store.database.Get(key(seenPrefix, path))
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(closer)

	if !bytes.Equal(saved, after) {
		t.Fatal("event replaced the scan marker")
	}
}
