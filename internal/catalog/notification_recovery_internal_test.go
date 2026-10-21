package catalog

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/resource"
)

func TestStagedChangeUsesLastObservationWithoutHash(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	store.Notifications.Use = nil
	original := workloadRecord(0)
	observeRecords(t, store, "initial", original)
	store.Notifications.Use = []string{fixtureDestinationFirst}

	err := store.startNotificationOperation(t.Context(), "update")
	if err != nil {
		t.Fatal(err)
	}

	changed := original
	changed.Mode = 0o700
	changed.Hash = workloadRecord(1).Hash
	observeRecords(t, store, "update", changed)
	changed.Hash, changed.Algorithm = "", ""
	changed.Ctime++
	observeRecords(t, store, "update", changed)

	detail := finishStagedDetail(t, store)

	if detail.Code() != "P" {
		t.Fatalf("last observation has no verified content change: %+v", detail)
	}
}

func TestRecreatedFileWithUnknownPreviousHashDoesNotProveContentChange(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	store.Notifications.Use = nil
	original := workloadRecord(0)
	before := original
	before.Hash, before.Algorithm = "", ""
	observeRecords(t, store, "initial", before)
	store.Notifications.Use = []string{fixtureDestinationFirst}

	err := store.startNotificationOperation(t.Context(), "update")
	if err != nil {
		t.Fatal(err)
	}

	observeRecords(t, store, "update", integrity.Record{Path: original.Path})
	original.Mode = 0o700
	observeRecords(t, store, "update", original)

	detail := finishStagedDetail(t, store)

	if detail.Code() != "P" {
		t.Fatalf("unknown previous content was classified as changed: %+v", detail)
	}
}

func TestRecreatedFileWithUnverifiedContentKeepsNotification(t *testing.T) {
	t.Parallel()

	for _, missingBefore := range []bool{true, false} {
		t.Run(strconv.FormatBool(missingBefore), func(t *testing.T) {
			t.Parallel()
			checkUnverifiedReplacement(t, missingBefore)
		})
	}
}

func checkUnverifiedReplacement(t *testing.T, missingBefore bool) {
	t.Helper()

	store := testStore(t)
	store.Notifications.Use = nil

	before, after := workloadRecord(0), workloadRecord(0)
	if missingBefore {
		before.Hash, before.Algorithm = "", ""
	} else {
		after.Hash, after.Algorithm = "", ""
	}

	observeRecords(t, store, "initial", before)
	store.Notifications.Use = []string{fixtureDestinationFirst}

	err := store.startNotificationOperation(t.Context(), "update")
	if err != nil {
		t.Fatal(err)
	}

	observeRecords(t, store, "update", integrity.Record{Path: before.Path})
	observeRecords(t, store, "update", after)

	detail := finishStagedDetail(t, store)

	if detail.Code() != "—" {
		t.Fatalf("unverified content was classified: %+v", detail)
	}
}

func TestStagedChangePreservesRoutingAcrossRestart(t *testing.T) {
	t.Parallel()

	for _, scenario := range []string{"disabled", "destination changed", "version changed"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			checkStagedRoutingRestart(t, scenario)
		})
	}
}

func checkStagedRoutingRestart(t *testing.T, scenario string) {
	t.Helper()

	const originalVersion = "original-version"

	path := filepath.Join(t.TempDir(), config.CatalogDirectory)
	store := testStoreAt(t, path)
	store.Notifications.Use = nil
	store.Notifications.BatchWindow = time.Nanosecond
	record := workloadRecord(0)
	observeRecords(t, store, "initial", record)
	store.Notifications.Use = []string{fixtureDestinationFirst}
	store.versions = map[string]string{fixtureDestinationFirst: originalVersion}

	err := store.startNotificationOperation(t.Context(), "update")
	if err != nil {
		t.Fatal(err)
	}

	record.Mode = 0o700
	observeRecords(t, store, "update", record)
	policy, notifications := store.policy, store.Notifications
	resource.Close(store)
	store.database = nil

	switch scenario {
	case "disabled":
		notifications.Use, notifications.Events = nil, nil
	case "destination changed":
		notifications.Use = []string{fixtureDestinationOther}
	}

	reopened, err := Open(t.Context(), path, "test", "host", policy, notifications)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(reopened)

	reopened.versions = map[string]string{fixtureDestinationFirst: "new-version"}

	err = reopened.startNotificationOperation(t.Context(), "next-update")
	if err != nil {
		t.Fatal(err)
	}

	deliveries, err := reopened.Due(t.Context())
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("pending delivery lost: count=%d err=%v", len(deliveries), err)
	}

	if delivery := deliveries[0]; delivery.Destination != fixtureDestinationFirst ||
		delivery.Version != originalVersion {
		t.Fatalf("pending delivery was rerouted: %+v", delivery)
	}
}

func finishStagedDetail(t *testing.T, store *Store) Detail {
	t.Helper()

	err := store.finishStagedChanges(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	deliveries, err := store.Due(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if len(deliveries) != 1 || len(deliveries[0].Message.Details) != 1 {
		t.Fatalf("want one delivery with one detail, got %+v", deliveries)
	}

	return deliveries[0].Message.Details[0]
}
