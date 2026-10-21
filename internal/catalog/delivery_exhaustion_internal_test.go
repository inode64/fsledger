package catalog

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/resource"
)

const exhaustionReason = "slack HTTP status 404"

// A delivery that keeps failing stops retrying but keeps its evidence: it leaves the schedule, so it
// delays no other delivery, and stays counted until outbox clear discards it.
func TestExhaustedDeliveryStaysQueuedWithoutBlockingOthers(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	store.Notifications.Use = []string{fixtureDestinationFirst}
	started := time.Unix(1000, 0)
	store.now = func() time.Time { return started }

	failing, healthy := announcePair(t, store)
	exhaustDelivery(t, store, failing)

	exhausted, err := store.RetryMany(t.Context(), []int64{failing}, time.Second, exhaustionReason)
	if err != nil || exhausted != 0 {
		t.Fatalf("exhausted delivery retried again: exhausted=%d error=%v", exhausted, err)
	}

	store.now = func() time.Time { return started.Add(time.Hour) }
	assertDue(t, store, healthy)
	assertExhaustedCounts(t, store, 2, 1)

	data, err := get(store.database, numberKey(outboxPrefix, failing))
	if err != nil {
		t.Fatal(err)
	}

	var delivery Delivery

	err = json.Unmarshal(data, &delivery)
	if err != nil || !delivery.Exhausted || delivery.Attempts != MaxDeliveryAttempts ||
		delivery.LastError != exhaustionReason {
		t.Fatalf("delivery=%+v error=%v", delivery, err)
	}

	discarded, err := store.DiscardPending(t.Context())
	if err != nil || discarded != 2 {
		t.Fatalf("discarded=%d error=%v", discarded, err)
	}

	assertExhaustedCounts(t, store, 0, 0)
}

// A restart gives exhausted deliveries a new retry cycle only while their frozen destination is configured
// unchanged; a changed destination never receives them.
func TestRestartRetriesExhaustedDeliveriesOfUnchangedDestinations(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), config.CatalogDirectory)
	original := config.Notifier{
		Type: config.NotifierSlack, URL: "https://hooks.example/original", DSN: "", From: "", Template: "", To: nil,
	}
	changed := original
	changed.URL = "https://hooks.example/changed"

	store := testStoreAt(t, path)
	store.Notifications.Use = []string{fixtureDestinationFirst}
	store.BindDestinations(map[string]config.Notifier{fixtureDestinationFirst: original})

	failing, healthy := announcePair(t, store)
	exhaustDelivery(t, store, failing)

	resource.Close(store)
	store.database = nil

	reopened := testStoreAt(t, path)
	reopened.now = func() time.Time { return time.Now().Add(time.Hour) }
	assertDue(t, reopened, healthy)

	// An earlier version rewrites the catalog state without the counter; the start recounts it.
	reopened.state.Stats.ExhaustedNotifications = 0

	for _, step := range []struct {
		notifier  config.Notifier
		retried   int
		exhausted int64
	}{{notifier: changed, retried: 0, exhausted: 1}, {notifier: original, retried: 1, exhausted: 0}} {
		reopened.BindDestinations(map[string]config.Notifier{fixtureDestinationFirst: step.notifier})

		retried, err := reopened.RetryExhausted(t.Context())
		if err != nil || retried != step.retried {
			t.Fatalf("retried=%d error=%v; want %d", retried, err, step.retried)
		}

		assertExhaustedCounts(t, reopened, 2, step.exhausted)
	}

	// The retried delivery is due from the restart on, after the one that never failed.
	due := assertDue(t, reopened, healthy, failing)
	if due[1].Exhausted || due[1].Attempts != 0 || due[1].LastError != exhaustionReason {
		t.Fatalf("retried delivery=%+v", due[1])
	}

	// Encoding keeps the format earlier versions read while nothing is exhausted.
	encoded, err := json.Marshal(due[1])
	if err != nil || bytes.Contains(encoded, []byte(`"exhausted"`)) {
		t.Fatalf("encoded=%s error=%v", encoded, err)
	}
}

func announcePair(t *testing.T, store *Store) (int64, int64) {
	t.Helper()

	for range 2 {
		err := store.Announce(t.Context(), config.NotificationError, "")
		if err != nil {
			t.Fatal(err)
		}
	}

	deliveries, err := store.Due(t.Context())
	if err != nil || len(deliveries) != 2 {
		t.Fatalf("deliveries=%d error=%v", len(deliveries), err)
	}

	return deliveries[0].ID, deliveries[1].ID
}

// exhaustDelivery fails one delivery until it stops retrying, which only the last failure reports.
func exhaustDelivery(t *testing.T, store *Store, identifier int64) {
	t.Helper()

	for attempt := 1; attempt <= MaxDeliveryAttempts; attempt++ {
		exhausted, err := store.RetryMany(t.Context(), []int64{identifier}, time.Second, exhaustionReason)
		if err != nil || (exhausted == 1) != (attempt == MaxDeliveryAttempts) {
			t.Fatalf("attempt %d: exhausted=%d error=%v", attempt, exhausted, err)
		}
	}
}

func assertDue(t *testing.T, store *Store, identifiers ...int64) []Delivery {
	t.Helper()

	due, err := store.Due(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	got := make([]int64, 0, len(due))
	for _, delivery := range due {
		got = append(got, delivery.ID)
	}

	if !slices.Equal(got, identifiers) {
		t.Fatalf("due=%v; want %v", got, identifiers)
	}

	return due
}

func assertExhaustedCounts(t *testing.T, store *Store, pending, exhausted int64) {
	t.Helper()

	stats, err := store.Stats(t.Context())
	if err != nil || stats.PendingNotifications != pending || stats.ExhaustedNotifications != exhausted {
		t.Fatalf("pending=%d exhausted=%d error=%v; want %d %d",
			stats.PendingNotifications, stats.ExhaustedNotifications, err, pending, exhausted)
	}
}
