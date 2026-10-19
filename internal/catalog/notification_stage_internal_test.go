package catalog

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/resource"
)

func TestRecreatedFilesInOneOperationNotifyNetModifications(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	store.Notifications.Use = nil
	store.Notifications.BatchWindow = time.Nanosecond

	const (
		count      = 488
		identifier = "plugin-update"
	)

	before, removed, after := pluginReplacementRecords(count)
	applyRecordsInBatches(t, store, "initial", before)

	store.Notifications.Use = []string{fixtureDestinationFirst}

	err := store.startNotificationOperation(t.Context(), identifier)
	if err != nil {
		t.Fatal(err)
	}

	applyRecordsInBatches(t, store, identifier, removed)
	applyRecordsInBatches(t, store, identifier, after)

	err = store.finishStagedChanges(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	store.notificationOperation = ""

	deliveries, err := store.Due(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	seen := make(map[string]bool, count)

	for _, delivery := range deliveries {
		for _, detail := range delivery.Message.Details {
			if detail.Kind != changeModified || detail.Code() != "M" || seen[string(detail.Path)] {
				t.Fatalf("incorrect net change: %+v", detail)
			}

			seen[string(detail.Path)] = true
		}
	}

	if len(seen) != count {
		t.Fatalf("got %d net changes, want %d", len(seen), count)
	}
}

func TestStagedChangesRetainRealAdditionsAndDeletions(t *testing.T) {
	t.Parallel()

	const identifier = "replacement"

	store := testStore(t)
	store.Notifications.Use = nil
	store.Notifications.BatchWindow = time.Nanosecond
	before := integrity.Record{Path: []byte("/deleted"), Type: fixtureRegular, Mode: 0o600}
	unchanged := integrity.Record{Path: []byte("/restored"), Type: fixtureRegular, Mode: 0o600}
	observeRecords(t, store, "initial", before, unchanged)

	store.Notifications.Use = []string{fixtureDestinationFirst}

	err := store.startNotificationOperation(t.Context(), identifier)
	if err != nil {
		t.Fatal(err)
	}

	observeRecords(t, store, identifier, integrity.Record{Path: before.Path}, integrity.Record{Path: unchanged.Path})
	observeRecords(t, store, identifier, unchanged,
		integrity.Record{Path: []byte("/created"), Type: fixtureRegular, Mode: 0o600})

	err = store.finishStagedChanges(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	store.notificationOperation = ""

	deliveries, err := store.Due(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	kinds := make(map[string]string)

	for _, delivery := range deliveries {
		for _, detail := range delivery.Message.Details {
			kinds[string(detail.Path)] = detail.Kind
		}
	}

	if len(kinds) != 2 || kinds["/deleted"] != changeDeleted || kinds["/created"] != changeAdded {
		t.Fatalf("incorrect final changes: %v", kinds)
	}
}

func TestStagedChangeSurvivesRestartBeforeNotification(t *testing.T) {
	t.Parallel()

	const identifier = "update"

	cfg, err := config.Load(testConfiguration(t))
	if err != nil {
		t.Fatal(err)
	}

	cfg.Integrity.Compare = []string{fieldHash, fixtureType, fixtureMode}
	cfg.Notifications.Use = nil
	cfg.Notifications.BatchWindow = time.Nanosecond
	path := filepath.Join(t.TempDir(), config.CatalogDirectory)

	store, err := Open(t.Context(), path, "test", "host", cfg.Integrity, cfg.Notifications)
	if err != nil {
		t.Fatal(err)
	}

	before := integrity.Record{
		Path: []byte("/plugin/file.php"), Type: fixtureRegular,
		Algorithm: fixtureSHA256, Hash: strings.Repeat("a", sha256.Size*2),
	}
	observeRecords(t, store, "initial", before)

	store.Notifications.Use = []string{fixtureDestinationFirst}

	err = store.startNotificationOperation(t.Context(), identifier)
	if err != nil {
		t.Fatal(err)
	}

	observeRecords(t, store, identifier, integrity.Record{Path: before.Path})
	after := before
	after.Hash = strings.Repeat("b", sha256.Size*2)
	observeRecords(t, store, identifier, after)

	err = store.Close()
	if err != nil {
		t.Fatal(err)
	}

	cfg.Notifications.Use = []string{fixtureDestinationFirst}

	reopened, err := Open(t.Context(), path, "test", "host", cfg.Integrity, cfg.Notifications)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(reopened)

	err = reopened.startNotificationOperation(t.Context(), "next-update")
	if err != nil {
		t.Fatal(err)
	}

	deliveries, err := reopened.Due(t.Context())
	if err != nil || len(deliveries) != 1 || len(deliveries[0].Message.Details) != 1 ||
		deliveries[0].Message.Details[0].Kind != changeModified {
		t.Fatalf("staged notification was not recovered: %+v %v", deliveries, err)
	}
}

func pluginReplacementRecords(count int) ([]integrity.Record, []integrity.Record, []integrity.Record) {
	before := make([]integrity.Record, count)
	removed := make([]integrity.Record, count)
	after := make([]integrity.Record, count)

	for index := range before {
		before[index] = integrity.Record{
			Path: []byte(fmt.Sprintf("/plugin/file-%03d.php", index)), Type: fixtureRegular,
			Algorithm: fixtureSHA256, Hash: strings.Repeat("a", sha256.Size*2),
		}
		removed[index] = integrity.Record{Path: before[index].Path}
		after[index] = before[index]
		after[index].Hash = strings.Repeat("b", sha256.Size*2)
	}

	return before, removed, after
}

func applyRecordsInBatches(t *testing.T, store *Store, identifier string, records []integrity.Record) {
	t.Helper()

	for start := 0; start < len(records); start += transactionBatchSize {
		end := min(start+transactionBatchSize, len(records))
		observeRecords(t, store, identifier, records[start:end]...)
	}
}
