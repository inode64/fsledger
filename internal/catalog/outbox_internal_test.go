package catalog

import (
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/resource"
)

// Queued messages must survive persistence with the kernel hostname and per-kind counts.
func TestMessageSummarizesKindsAndHostname(t *testing.T) {
	t.Parallel()

	hostname, err := os.Hostname()
	if err != nil {
		t.Skip("kernel hostname unavailable:", err)
	}

	store := testStore(t)
	kinds := []string{changeAdded, changeAdded, changeModified, changeModified, changeModified, changeDeleted}
	details := make([]Detail, 0, len(kinds))

	for index, kind := range kinds {
		details = append(details, Detail{
			ChangeID: "",
			Path:     []byte(fmt.Sprintf("/path/%d", index)),
			Kind:     kind,
			Actor:    fixtureActorUnknown,
			Fields:   nil,
			Baseline: nil,
			Observed: time.Now().UnixNano(),
		})
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	store.enqueue(transaction, config.NotificationChange, "change", details)

	err = transaction.commit()
	if err != nil {
		t.Fatal(err)
	}

	err = store.Announce(t.Context(), config.NotificationError, "")
	if err != nil {
		t.Fatal(err)
	}

	deliveries, err := store.Due(t.Context())
	if err != nil || len(deliveries) != 4 {
		t.Fatalf("deliveries: %d %v", len(deliveries), err)
	}

	for _, delivery := range deliveries {
		message := delivery.Message
		want := [3]int{2, 3, 1}

		if message.Event == config.NotificationError {
			want = [3]int{}
		}

		got := [3]int{message.Added(), message.Modified(), message.Deleted()}
		if message.Hostname != hostname || got != want {
			t.Fatalf("%s: hostname %q counts %v; want %q %v", message.Event, message.Hostname, got, hostname, want)
		}
	}
}

//nolint:funlen // Ordered timeline fixture makes window transitions explicit.
func TestNotificationWindowsAlignGrowAndReset(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	store.Notifications.Use = []string{fixtureDestinationFirst}
	store.Notifications.BatchWindow = time.Minute
	store.Notifications.MaxBatchWindow = 15 * time.Minute

	started := time.Unix(1000, 0)
	now := started
	store.now = func() time.Time { return now }

	queue := func(offset time.Duration) {
		t.Helper()

		now = started.Add(offset)
		transaction := store.begin()

		defer resource.Close(transaction.batch)

		store.enqueue(
			transaction,
			config.NotificationChange,
			offset.String(),
			[]Detail{notificationTestDetail("/path/" + offset.String())},
		)

		err := transaction.commit()
		if err != nil {
			t.Fatal(err)
		}
	}

	queue(0)
	queue(30 * time.Second)
	queue(123 * time.Second)
	queue(200 * time.Second)
	queue(369 * time.Second)
	queue(22 * time.Minute)

	now = started.Add(time.Hour)

	deliveries, err := store.Due(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	want := []time.Duration{
		time.Minute, time.Minute,
		243 * time.Second, 243 * time.Second,
		609 * time.Second,
		23 * time.Minute,
	}
	if len(deliveries) != len(want) {
		t.Fatalf("deliveries=%d, want %d", len(deliveries), len(want))
	}

	for index, delivery := range deliveries {
		got := time.Unix(0, delivery.NextAttempt).Sub(started)
		if got != want[index] {
			t.Fatalf("delivery %d due at %s, want %s", index, got, want[index])
		}
	}
}

func TestNotificationWindowGrowthCanBeDisabled(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	store.Notifications.Use = []string{fixtureDestinationFirst}
	store.Notifications.BatchWindow = time.Minute
	store.Notifications.MaxBatchWindow = time.Minute

	started := time.Unix(1000, 0)
	now := started
	store.now = func() time.Time { return now }

	for index, offset := range []time.Duration{0, 123 * time.Second} {
		now = started.Add(offset)
		transaction := store.begin()

		identifier := strconv.Itoa(index)
		store.enqueue(
			transaction,
			config.NotificationChange,
			identifier,
			[]Detail{notificationTestDetail("/path/" + identifier)},
		)

		err := transaction.commit()
		if err != nil {
			t.Fatal(err)
		}

		resource.Close(transaction.batch)
	}

	now = started.Add(time.Hour)

	deliveries, err := store.Due(t.Context())
	if err != nil || len(deliveries) != 2 {
		t.Fatal(deliveries, err)
	}

	for index, want := range []time.Duration{time.Minute, 183 * time.Second} {
		if got := time.Unix(0, deliveries[index].NextAttempt).Sub(started); got != want {
			t.Fatalf("delivery %d due at %s, want %s", index, got, want)
		}
	}
}

func TestDueReturnsUpToDigestReadLimit(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	store.Notifications.Use = []string{fixtureDestinationFirst}

	for range 1100 {
		err := store.Announce(t.Context(), config.NotificationError, "")
		if err != nil {
			t.Fatal(err)
		}
	}

	deliveries, err := store.Due(t.Context())
	if err != nil || len(deliveries) != 1024 {
		t.Fatalf("deliveries=%d error=%v", len(deliveries), err)
	}
}

func notificationTestDetail(path string) Detail {
	return Detail{
		ChangeID: "", Kind: changeModified, Actor: fixtureActorUnknown, Path: []byte(path),
		Fields: nil, Baseline: nil, Observed: 0,
	}
}

// Operational events must say why they were raised: an error without a reason is noise.
func TestAnnounceCarriesReason(t *testing.T) {
	t.Parallel()
	store := testStore(t)

	const reason = "fanotify event loss or coverage loss; reconciliation required"

	err := store.Announce(t.Context(), config.NotificationError, reason)
	if err != nil {
		t.Fatal(err)
	}

	deliveries, err := store.Due(t.Context())
	if err != nil || len(deliveries) == 0 {
		t.Fatalf("deliveries: %d %v", len(deliveries), err)
	}

	for _, delivery := range deliveries {
		if delivery.Message.Event != config.NotificationError || delivery.Message.Reason != reason {
			t.Fatalf("announcement lost its reason: %+v", delivery.Message)
		}
	}
}

// Clearing a queue must reach delayed retries too, keep the counter exact and leave the store usable.
func TestDiscardPendingDropsDueAndDelayedDeliveries(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	store.Notifications.Use = []string{fixtureDestinationFirst}

	for range 3 {
		err := store.Announce(t.Context(), config.NotificationError, "")
		if err != nil {
			t.Fatal(err)
		}
	}

	deliveries, err := store.Due(t.Context())
	if err != nil || len(deliveries) != 3 {
		t.Fatalf("deliveries=%d error=%v", len(deliveries), err)
	}

	err = store.RetryMany(t.Context(), []int64{deliveries[0].ID}, time.Hour, "SMTP unavailable")
	if err != nil {
		t.Fatal(err)
	}

	discarded, err := store.DiscardPending(t.Context())
	if err != nil || discarded != 3 {
		t.Fatalf("discarded=%d error=%v", discarded, err)
	}

	stats, err := store.Stats(t.Context())
	if err != nil || stats.PendingNotifications != 0 {
		t.Fatalf("pending=%d error=%v", stats.PendingNotifications, err)
	}

	remaining := 0

	err = store.iterate(t.Context(), outboxPrefix, func(_, _ []byte) error {
		remaining++

		return nil
	})
	if err != nil || remaining != 0 {
		t.Fatalf("outbox keeps %d deliveries: %v", remaining, err)
	}

	err = store.Announce(t.Context(), config.NotificationError, "")
	if err != nil {
		t.Fatal(err)
	}

	deliveries, err = store.Due(t.Context())
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("queue unusable after discard: deliveries=%d error=%v", len(deliveries), err)
	}
}
