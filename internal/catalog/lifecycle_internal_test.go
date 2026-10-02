package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	fixtureType             = "type"
	fixtureMode             = "mode"
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

	cfg, err := config.Load(testConfiguration(t))
	if err != nil {
		t.Fatal(err)
	}

	cfg.Integrity.Compare = []string{"hash", fixtureType, "uid", "gid", fixtureMode}
	cfg.Notifications.Use = []string{fixtureDestinationFirst, fixtureDestinationOther}
	cfg.Notifications.BatchWindow = 0

	store, err := Open(
		t.Context(),
		path,
		"test",
		"host",
		cfg.Integrity,
		cfg.Notifications,
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

	_, _, err := store.apply(t.Context(), updates, 1, identifier, "test actor")
	if err != nil {
		t.Fatal(err)
	}
}

func reference(t *testing.T, store *Store, records ...integrity.Record) {
	t.Helper()
	observeRecords(t, store, "initial", records...)

	store.state.Complete = true

	err := store.InitBaseline(t.Context())
	if err != nil {
		t.Fatal(err)
	}
}

func pendingIDs(t *testing.T, store *Store) map[string]string {
	t.Helper()

	result := make(map[string]string)

	err := store.Changes(
		t.Context(),
		100,
		func(id string, path []byte, _ string, _ []byte) error {
			result[string(path)] = id

			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	return result
}

//nolint:cyclop,funlen,gocognit,gocyclo // Ordered stale approval, cleanup and delivery lifecycle.
func TestStaleApprovalAndNoHistory(t *testing.T) {
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
	reference(t, store, first, second)
	first.Hash, second.Hash = strings.Repeat("b", 64), strings.Repeat("c", 64)
	observeRecords(t, store, "both", first, second)
	original := pendingIDs(t, store)
	first.Hash = strings.Repeat("d", 64)
	observeRecords(t, store, "newer", first)

	operationErr1 := store.Accept(t.Context(), "both")
	if operationErr1 == nil {
		t.Fatal("partly stale batch accepted")
	}

	operationErr2 := store.Accept(t.Context(), original["/one"])
	if operationErr2 == nil {
		t.Fatal("stale file version accepted")
	}

	operationErr3 := store.Accept(t.Context(), original["/two"])
	if operationErr3 != nil {
		t.Fatal(operationErr3)
	}

	operationErr4 := store.Accept(t.Context(), "newer")
	if operationErr4 != nil {
		t.Fatal(operationErr4)
	}

	stats, err := store.Stats(t.Context())
	if err != nil || stats.Violations != 0 {
		t.Fatalf("pending state retained: %+v %v", stats, err)
	}

	for _, prefix := range []string{pendingPrefix, tokenPrefix, approvalPrefix} {
		err = store.iterate(
			t.Context(),
			prefix,
			func(_, _ []byte) error {
				t.Errorf("resolved data retained in %s", prefix)

				return nil
			},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

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

	stats, err = store.Stats(t.Context())
	if err != nil || stats.PendingNotifications != 0 {
		t.Fatal("delivered evidence retained", err)
	}
}

func TestResolvedDifferenceDisappears(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	original := integrity.Record{Path: []byte("/file"), Type: fixtureRegular, Mode: 0o600}
	reference(t, store, original)
	changed := original
	changed.Mode = 0o700
	observeRecords(t, store, "changed", changed)
	observeRecords(t, store, "restored", original)

	if len(pendingIDs(t, store)) != 0 {
		t.Fatal("resolved difference retained")
	}

	err := store.Accept(t.Context(), "changed")
	if err == nil {
		t.Fatal("resolved ID accepted")
	}
}

func TestReferenceReplacementCancellation(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	original := integrity.Record{Path: []byte("/file"), Type: fixtureRegular, Mode: 0o600}
	reference(t, store, original)
	changed := original
	changed.Mode = 0o700
	observeRecords(t, store, "changed", changed)
	store.state.Complete = true
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	operationErr5 := store.ReplaceBaseline(ctx)
	if operationErr5 == nil {
		t.Fatal("cancelled reference published")
	}

	record, _, err := readRecord(store.database, baselinePrefix(store.state.Baseline), original.Path)
	if err != nil || record.Mode != original.Mode {
		t.Fatal("original reference lost", err)
	}

	err = store.ReplaceBaseline(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if len(pendingIDs(t, store)) != 0 {
		t.Fatal("replacement retained old differences")
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
		TimesInSeconds:   true,
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

func TestImportKeepsOriginalEvidence(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	store.Notifications.Use = nil
	original := integrity.Record{
		Path:      []byte("/file"),
		Type:      fixtureRegular,
		Algorithm: fixtureSHA256,
		Hash:      strings.Repeat("a", 64),
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}

	err = store.ImportBaseline(t.Context(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	changed := original
	changed.Hash = strings.Repeat("b", 64)
	observeRecords(t, store, "changed", changed)

	if len(pendingIDs(t, store)) != 1 {
		t.Fatal("import approved current data")
	}

	err = store.ImportBaseline(t.Context(), bytes.NewReader(data))
	if err == nil {
		t.Fatal("nonempty import allowed")
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
		other, err := Open(t.Context(), path, "test", "host", store.Policy, store.Notifications)
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
		store.Policy,
		store.Notifications,
	)
	if err == nil {
		resource.Close(other)
		t.Fatal("existing file silently replaced")
	}
}

func TestLargeApprovalRollsBackAtomically(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	store.Notifications.Use = nil

	records := make([]integrity.Record, approvalPathLimit+1)
	for index := range records {
		records[index] = integrity.Record{
			Path: []byte(fmt.Sprintf("/file-%05d", index)),
			Type: fixtureRegular,
			Mode: 0o600,
		}
	}

	reference(t, store, records...)

	for index := range records {
		records[index].Mode = 0o700
	}

	observeRecords(t, store, "too-large", records...)

	err := store.Accept(t.Context(), "too-large")
	if err == nil {
		t.Fatal("unbounded approval accepted")
	}

	original, _, err := readRecord(store.database, baselinePrefix(store.state.Baseline), records[0].Path)
	if err != nil || original.Mode != 0o600 {
		t.Fatal("failed approval changed reference", err)
	}

	stats, err := store.Stats(t.Context())
	if err != nil || stats.Violations != int64(len(records)) {
		t.Fatal("failed approval removed pending differences", stats, err)
	}
}
