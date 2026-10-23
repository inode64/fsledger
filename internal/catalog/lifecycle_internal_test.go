package catalog

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/resource"
)

const (
	fixtureRegular          = "regular"
	fixtureSHA256           = "sha256"
	fixtureDestinationFirst = "first"
	fixtureDestinationOther = "second"
	fixtureActorUnknown     = "unknown"
)

func testStore(t *testing.T) *Store {
	t.Helper()

	return testStoreAt(t, filepath.Join(t.TempDir(), config.CatalogDirectory))
}

func testStoreAt(t *testing.T, path string) *Store {
	t.Helper()
	cfg := testStoreConfig(t)

	return openTestStore(t, path, cfg.Integrity, cfg.Notifications)
}

func testStoreConfig(t *testing.T) *config.Config {
	t.Helper()

	cfg, err := config.Load(testConfiguration(t))
	if err != nil {
		t.Fatal(err)
	}

	cfg.Notifications.Use = []string{fixtureDestinationFirst, fixtureDestinationOther}
	cfg.Notifications.BatchWindow = 0

	return cfg
}

func openTestStore(t *testing.T, path string, policy config.Integrity, notifications config.Notifications) *Store {
	t.Helper()

	store, err := Open(
		t.Context(),
		path,
		"test",
		"host",
		policy,
		notifications,
	)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if store.database != nil {
			resource.Close(store)
		}
	})

	return store
}

func reopenTestStore(t *testing.T, store *Store, path string, policy config.Integrity) *Store {
	t.Helper()

	err := store.Close()
	if err != nil {
		t.Fatal(err)
	}

	store.database = nil

	return openTestStore(t, path, policy, store.Notifications)
}

func observeRecords(t *testing.T, store *Store, identifier string, records ...integrity.Record) {
	t.Helper()

	updates := make([]mutation, len(records))
	for index, record := range records {
		updates[index] = mutation{
			refreshHash: false, excluded: false,
			record:   record,
			previous: nil,
			deleted:  record.Type == "",
		}
	}

	// Production applies inside an operation that publishes its staged changes when it ends.
	_, err := store.withNotificationOperation(t.Context(), identifier, func(operation string) (Result, error) {
		_, applyErr := store.apply(t.Context(), updates, 1, operation, "test actor")

		return Result{ID: operation}, applyErr
	})
	if err != nil {
		t.Fatal(err)
	}
}

// stageRecords applies records inside an operation still in progress: their changes stay staged, as a crash
// before the operation ends leaves them.
func stageRecords(t *testing.T, store *Store, identifier string, records ...integrity.Record) {
	t.Helper()

	updates := make([]mutation, len(records))
	for index, record := range records {
		updates[index] = mutation{
			refreshHash: false, excluded: false, record: record, previous: nil, deleted: record.Type == "",
		}
	}

	_, err := store.apply(t.Context(), updates, 1, identifier, "test actor")
	if err != nil {
		t.Fatal(err)
	}
}

func observeInitial(t *testing.T, store *Store, records ...integrity.Record) {
	t.Helper()
	observeRecords(t, store, "initial", records...)
}

// Every delivery carries its own evidence: no change history is kept once it is delivered.
func TestDeliveriesCarryEvidenceWithoutHistory(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	first := integrity.Record{
		Path:      []byte("/one"),
		Type:      fixtureRegular,
		Algorithm: fixtureSHA256,
		Hash:      strings.Repeat("a", 64),
	}
	second := first
	second.Path = []byte("/two")
	observeInitial(t, store, first, second)
	first.Hash, second.Hash = strings.Repeat("b", 64), strings.Repeat("c", 64)
	observeRecords(t, store, "both", first, second)

	due, err := store.Due(t.Context())
	if err != nil || len(due) == 0 {
		t.Fatal("missing durable evidence", err)
	}

	for _, delivery := range due {
		if len(delivery.Message.Details) == 0 {
			t.Fatal("notification depends on removed history")
		}

		err = store.DeliveredMany(t.Context(), []int64{delivery.ID})
		if err != nil {
			t.Fatal(err)
		}
	}

	stats, err := store.Stats(t.Context())
	if err != nil || stats.PendingNotifications != 0 {
		t.Fatal("delivered evidence retained", err)
	}
}

func TestBinaryMetadataCodec(t *testing.T) {
	t.Parallel()

	record := integrity.Record{
		Path:             []byte{'/', 255},
		Type:             fixtureRegular,
		Algorithm:        fixtureSHA256,
		Hash:             strings.Repeat("ab", 32),
		Size:             -1,
		Inode:            ^uint64(0),
		Atime:            -1,
		Mtime:            2,
		Ctime:            3,
		Btime:            4,
		HasBtime:         true,
		HashedAt:         5,
		Observed:         6,
		Nlink:            7,
		Device:           8,
		UID:              9,
		GID:              10,
		Mode:             0o600,
		NoAtime:          true,
		Target:           []byte{255},
		AttributesStatus: "unsupported",
		ACL:              []integrity.Attribute{{Name: []byte{254}, Value: []byte{0, 255}}},
		Xattrs:           []integrity.Attribute{{Name: []byte("user.test"), Value: []byte{0, 1}}},
	}

	data, err := packRecord(record)
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := unpack(data, record.Path)
	if err != nil || !reflect.DeepEqual(record, decoded) {
		t.Fatalf("metadata loss: %+v %v", decoded, err)
	}

	_, err = unpack([]byte{255}, record.Path)
	if err == nil {
		t.Fatal("corrupt protobuf accepted")
	}
}

func TestCatalogRejectsSymlinkAndFiles(t *testing.T) {
	t.Parallel()
	store := testStore(t)

	root := t.TempDir()

	operationErr6 := os.Symlink(root, filepath.Join(root, "link"))
	if operationErr6 != nil {
		t.Fatal(operationErr6)
	}

	for _, path := range []string{filepath.Join(root, "link", "catalog"), filepath.Join(root, "link")} {
		other, err := Open(t.Context(), path, "test", "host", store.policy, store.Notifications)
		if err == nil {
			resource.Close(other)
			t.Fatal("symlink accepted")
		}
	}

	operationErr7 := os.WriteFile(filepath.Join(root, config.CatalogDirectory), []byte("occupied"), 0o600)
	if operationErr7 != nil {
		t.Fatal(operationErr7)
	}

	other, err := Open(
		t.Context(),
		filepath.Join(root, config.CatalogDirectory),
		"test",
		"host",
		store.policy,
		store.Notifications,
	)
	if err == nil {
		resource.Close(other)
		t.Fatal("existing file silently replaced")
	}
}
