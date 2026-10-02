package catalog

import (
	"fmt"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
)

func TestMergeMessagesGroupsInterleavedEvidence(t *testing.T) {
	t.Parallel()

	deliveries := make([]Delivery, 0, 600)

	for index := range 300 {
		for _, event := range []string{config.NotificationChange, config.NotificationIntegrityViolation} {
			identifier := fmt.Sprintf("change-%d", index)
			deliveries = append(deliveries, Delivery{
				Destination: "sink", Version: "v1", LastError: "",
				ID: int64(len(deliveries) + 1), Attempts: 0, NextAttempt: 0,
				Message: Message{
					Repository: "repo", Host: "host", Hostname: "kernel", Event: event,
					ChangeID: identifier, Paths: []string{fmt.Sprintf("/path/%d", index)}, Count: 1,
					Details: []Detail{{
						ChangeID: "", Path: []byte(fmt.Sprintf("/path/%d", index)), Kind: changeModified,
						Actor: fixtureActorUnknown, Fields: nil, Baseline: nil, Observed: 0,
					}},
				},
			})
		}
	}

	digests := MergeMessages(deliveries)
	if len(digests) != 2 {
		t.Fatalf("digests=%d, want 2", len(digests))
	}

	for _, digest := range digests {
		if len(digest.Deliveries) != 300 || digest.Message.Count != 300 ||
			len(digest.Message.Paths) != 300 || len(digest.Message.Details) != 300 ||
			digest.Message.ChangeID != "" {
			t.Fatal("incomplete digest", digest.Message, len(digest.Deliveries))
		}

		for _, detail := range digest.Message.Details {
			if detail.ChangeID == "" {
				t.Fatal("missing per-detail change ID")
			}
		}
	}
}

func TestMergeMessagesKeepsRoutingAndOperationalEventsSeparate(t *testing.T) {
	t.Parallel()

	base := Delivery{
		Destination: "one", Version: "v1", LastError: "", ID: 1, Attempts: 0, NextAttempt: 0,
		Message: Message{Repository: "repo", Host: "host", Hostname: "kernel", Event: config.NotificationChange},
	}
	deliveries := []Delivery{
		base,
		withDelivery(base, 2, "two", "v1", config.NotificationChange),
		withDelivery(base, 3, "one", "v2", config.NotificationChange),
		withDelivery(base, 4, "one", "v1", config.NotificationError),
		withDelivery(base, 5, "one", "v1", config.NotificationError),
	}

	digests := MergeMessages(deliveries)
	if len(digests) != len(deliveries) {
		t.Fatalf("digests=%d, want %d", len(digests), len(deliveries))
	}
}

func withDelivery(base Delivery, id int64, destination, version, event string) Delivery {
	base.ID = id
	base.Destination = destination
	base.Version = version
	base.Message.Event = event

	return base
}

//nolint:cyclop,gocognit,gocyclo // Ordered persistence fixture verifies every batch transition.
func TestDeliveredAndRetryManyAreAtomic(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	store.Notifications.Use = []string{fixtureDestinationFirst}

	for range 3 {
		err := store.Announce(t.Context(), config.NotificationError, "")
		if err != nil {
			t.Fatal(err)
		}
	}

	due, err := store.Due(t.Context())
	if err != nil || len(due) != 3 {
		t.Fatal(due, err)
	}

	ids := []int64{due[0].ID, due[1].ID}

	err = store.RetryMany(t.Context(), ids, 1, "temporary")
	if err != nil {
		t.Fatal(err)
	}

	store.now = func() time.Time { return time.Now().Add(time.Hour) }

	due, err = store.Due(t.Context())
	if err != nil || len(due) != 3 {
		t.Fatal(due, err)
	}

	for _, delivery := range due {
		if delivery.ID != ids[0] && delivery.ID != ids[1] {
			continue
		}

		if delivery.Attempts != 1 || delivery.LastError != "temporary" {
			t.Fatal("retry state not updated", delivery)
		}
	}

	err = store.DeliveredMany(t.Context(), ids)
	if err != nil {
		t.Fatal(err)
	}

	stats, err := store.Stats(t.Context())
	if err != nil || stats.PendingNotifications != 1 {
		t.Fatal(stats, err)
	}
}
