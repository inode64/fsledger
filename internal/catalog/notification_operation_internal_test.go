package catalog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/integrity"
)

func TestObserveRecreatedFileNotifiesOneModification(t *testing.T) {
	t.Parallel()
	store, scanner, matcher, root := subtreeFixture(t)
	store.Notifications.Use = []string{fixtureDestinationFirst}
	store.Notifications.Events = []string{config.NotificationChange}
	path := filepath.Join(root, "a", "kept")
	trigger := filepath.Join(root, "direct-before")

	err := os.Remove(path)
	if err != nil {
		t.Fatal(err)
	}

	var recreateErr error

	store.BindCopiedHashes(func(record integrity.Record, _ string) (string, int64, bool) {
		if string(record.Path) == trigger {
			recreateErr = os.WriteFile(path, []byte("replacement"), 0o600)
		}

		return "", 0, false
	})

	const identifier = "recreated-file"

	_, err = store.Observe(t.Context(), scanner, []string{filepath.Dir(path), path, trigger},
		[]string{root}, matcher, "actor", identifier)
	if err != nil || recreateErr != nil {
		t.Fatal(err, recreateErr)
	}

	details := operationDetails(t, store, identifier)
	if len(details) != 1 || string(details[0].Path) != path || details[0].Code() != "M" {
		t.Fatalf("want one modification for the recreated file, got %+v", details)
	}
}

func TestCancelledNotificationOperationRecoversCommittedChangesOnce(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	store.Notifications.Use = nil
	record := workloadRecord(0)
	observeRecords(t, store, "initial", record)
	store.Notifications.Use = []string{fixtureDestinationFirst}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const identifier = "cancelled-update"

	_, err := store.withNotificationOperation(ctx, identifier, func(operationID string) (Result, error) {
		record.Mode = 0o700
		_, applyErr := store.apply(ctx, []mutation{{
			record: record, previous: nil, deleted: false, excluded: false, refreshHash: false,
		}}, 1, operationID, "test actor")

		cancel()

		return Result{ID: operationID}, errors.Join(applyErr, ctx.Err())
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled operation was not released", err)
	}

	if details := operationDetails(t, store, identifier); len(details) != 0 {
		t.Fatal("cancelled publication should leave durable staging", details)
	}

	for range 2 {
		_, err = store.withNotificationOperation(t.Context(), "recovery", func(operationID string) (Result, error) {
			return Result{ID: operationID}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	details := operationDetails(t, store, identifier)
	if len(details) != 1 || details[0].Code() != "P" {
		t.Fatalf("want exactly one recovered permission change, got %+v", details)
	}
}

func operationDetails(t *testing.T, store *Store, identifier string) []Detail {
	t.Helper()

	deliveries, err := store.Due(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	var details []Detail

	for _, delivery := range deliveries {
		if delivery.Message.Event == config.NotificationChange && delivery.Message.ChangeID == identifier {
			details = append(details, delivery.Message.Details...)
		}
	}

	return details
}
