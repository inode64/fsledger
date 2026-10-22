package catalog

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/resource"
)

func TestNotificationBatchesPreserveEveryChangeID(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	store.Notifications.BatchWindow = time.Nanosecond
	queueBatchFixture(t, store, 130)

	deliveries, err := store.Due(t.Context())
	if err != nil || len(deliveries) != 6 {
		t.Fatalf("bounded batches: %d %v", len(deliveries), err)
	}

	checkBatchEvidence(t, deliveries, 130)
	// A sender may hold these deliveries while another transaction commits.
	queueBatchFixture(t, store, 1)

	for _, delivery := range deliveries {
		err = store.DeliveredMany(t.Context(), []int64{delivery.ID})
		if err != nil {
			t.Fatal(err)
		}
	}

	remaining, err := store.Due(t.Context())
	if err != nil || len(remaining) != 2 {
		t.Fatal("acknowledgement erased newly queued evidence", remaining, err)
	}
}

func queueBatchFixture(t *testing.T, store *Store, count int) {
	t.Helper()

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	for index := range count {
		detail := Detail{
			ChangeID:    "",
			Path:        []byte(fmt.Sprintf("/path/%d", index)),
			Kind:        "modified",
			Actor:       fixtureActorUnknown,
			Fields:      nil,
			Observed:    time.Now().UnixNano(),
			BackdatedBy: 0,
		}
		enqueueObserved(store, transaction, fmt.Sprintf("change-%d", index), detail)
	}

	err := transaction.commit()
	if err != nil {
		t.Fatal(err)
	}
}

func checkBatchEvidence(t *testing.T, deliveries []Delivery, count int) {
	t.Helper()

	seen := make(map[string]bool)

	for _, delivery := range deliveries {
		if delivery.Message.Count != len(delivery.Message.Details) || delivery.Message.Count > notificationBatchPaths {
			t.Fatal(delivery.Message)
		}

		if delivery.Destination != fixtureDestinationFirst {
			continue
		}

		for _, detail := range delivery.Message.Details {
			if detail.ChangeID == "" || seen[detail.ChangeID] {
				t.Fatal("missing or repeated evidence", detail)
			}

			seen[detail.ChangeID] = true
		}
	}

	if len(seen) != count {
		t.Fatal("lost evidence", len(seen))
	}
}

func TestNotificationBatchByteLimitAppliesToEveryDestination(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	store.Notifications.BatchWindow = time.Nanosecond

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	for index := range 3 {
		detail := Detail{
			ChangeID: "", Path: []byte(fmt.Sprintf("/large/%d", index)), Kind: "modified",
			Actor: strings.Repeat("x", notificationBatchBytes/2), Fields: nil, Observed: 1,
			BackdatedBy: 0,
		}
		enqueueObserved(store, transaction, fmt.Sprintf("large-%d", index), detail)
	}

	err := transaction.commit()
	if err != nil {
		t.Fatal(err)
	}

	deliveries, err := store.Due(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	counts := make(map[string]int)

	for _, delivery := range deliveries {
		if delivery.Message.Count != 1 || len(delivery.Message.Details) != 1 {
			t.Fatal("byte limit did not split large evidence", delivery.Message)
		}

		counts[delivery.Destination]++
	}

	for _, destination := range store.Notifications.Use {
		if counts[destination] != 3 {
			t.Fatalf("%s received %d batches", destination, counts[destination])
		}
	}
}
