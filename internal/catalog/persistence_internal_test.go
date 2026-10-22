package catalog

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"

	"github.com/inode64/fsledger/internal/integrity"

	"github.com/inode64/fsledger/internal/resource"
)

func TestUnchangedObservationOnlyWritesScanMarker(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	record := workloadRecord(0)
	observeInitial(t, store, record)
	record.Observed++

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	changed, err := store.applyOne(
		t.Context(),
		transaction,
		mutation{refreshHash: false, excluded: false, record: record, previous: nil, deleted: false},
		2,
		"unchanged",
		"test",
	)
	if err != nil || changed {
		t.Fatal("unchanged observation differs", err)
	}
	// With no differences, the batch should contain just the scan generation marker.
	reader := transaction.batch.Reader()

	_, lookup, value, valid, err := reader.Next()
	if err != nil || !valid || !bytes.Equal(lookup, key(seenPrefix, record.Path)) ||
		binary.BigEndian.Uint64(value) != 2 {
		t.Fatal("unchanged observation rewrote metadata instead of only marking it seen", string(lookup), err)
	}

	_, lookup, _, valid, err = reader.Next()
	if err != nil || valid {
		t.Fatal("unexpected extra write", string(lookup), err)
	}
}

func TestPersistenceKeepsHashFreshnessAndUncomparedMetadata(t *testing.T) {
	t.Parallel()

	for name, change := range map[string]func(*integrity.Record){
		"hash freshness": func(record *integrity.Record) { record.HashedAt++ },
		"size":           func(record *integrity.Record) { record.Size++ },
		"xattrs": func(record *integrity.Record) {
			record.Xattrs = []integrity.Attribute{{Name: []byte("user.test"), Value: []byte("value")}}
		},
		"mtime": func(record *integrity.Record) { record.Mtime++ },
		"inode": func(record *integrity.Record) { record.Inode++ },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := testStore(t)
			record := workloadRecord(0)
			observeInitial(t, store, record)

			before, err := store.Due(t.Context())
			if err != nil {
				t.Fatal(err)
			}

			change(&record)
			record.Observed++
			observeRecords(t, store, "metadata", record)

			current, exists, err := store.Current(t.Context(), record.Path)
			if err != nil || !exists || !reflect.DeepEqual(current, record) {
				t.Fatalf("metadata lost: %+v %v", current, err)
			}

			after, err := store.Due(t.Context())
			if err != nil || len(after) != len(before) {
				t.Fatal("metadata outside the policy announced a change", len(before), len(after), err)
			}
		})
	}
}
