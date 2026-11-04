package catalog

import (
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/resource"
)

// debounceClock drives one store minute by minute, as the sender polls it, and records when each
// delivery is handed out.
type debounceClock struct {
	t       *testing.T
	store   *Store
	started time.Time
	now     time.Time
}

func newDebounceClock(t *testing.T) *debounceClock {
	t.Helper()

	store := testStore(t)
	store.Notifications.Use = []string{fixtureDestinationFirst}
	store.Notifications.BatchWindow = time.Minute
	store.Notifications.MaxBatchWindow = 15 * time.Minute
	store.Notifications.Debounce = 5 * time.Minute

	clock := &debounceClock{t: t, store: store, started: time.Unix(1000, 0), now: time.Unix(1000, 0)}
	store.now = func() time.Time { return clock.now }

	return clock
}

func (clock *debounceClock) queue(offset time.Duration) {
	clock.t.Helper()

	clock.now = clock.started.Add(offset)
	transaction := clock.store.begin()

	defer resource.Close(transaction.batch)

	identifier := strconv.Itoa(int(offset / time.Second))
	enqueueObserved(clock.store, transaction, identifier, notificationTestDetail("/path/"+identifier))

	err := transaction.commit()
	if err != nil {
		clock.t.Fatal(err)
	}
}

// poll returns the deliveries due at the given offset and acknowledges them, as a successful send does.
func (clock *debounceClock) poll(offset time.Duration) []Delivery {
	clock.t.Helper()

	clock.now = clock.started.Add(offset)

	deliveries, err := clock.store.Due(clock.t.Context())
	if err != nil {
		clock.t.Fatal(err)
	}

	identifiers := make([]int64, 0, len(deliveries))
	for _, delivery := range deliveries {
		identifiers = append(identifiers, delivery.ID)
	}

	err = clock.store.DeliveredMany(clock.t.Context(), identifiers)
	if err != nil {
		clock.t.Fatal(err)
	}

	return deliveries
}

// run queues a change at each event minute and polls every minute up to end. It returns, per minute
// with deliveries, how many were handed out.
func (clock *debounceClock) run(events []int, end int) map[int]int {
	clock.t.Helper()

	sent := make(map[int]int)

	for minute := 0; minute <= end; minute++ {
		offset := time.Duration(minute) * time.Minute
		if slices.Contains(events, minute) {
			clock.queue(offset)
		}

		if deliveries := clock.poll(offset); len(deliveries) > 0 {
			sent[minute] = len(deliveries)
		}
	}

	return sent
}

func TestNotificationDebounceWaitsForQuietPeriod(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		want   map[int]int
		name   string
		events []int
	}{
		{
			name:   "five quiet minutes after the last change",
			events: []int{0, 2, 4},
			want:   map[int]int{9: 3},
		},
		{
			// Changes every four minutes never leave five quiet ones: max_batch_window bounds the wait
			// from the first change, and the next change opens a new window.
			name:   "bounded by max_batch_window",
			events: []int{0, 4, 8, 12, 16},
			want:   map[int]int{15: 4, 21: 1},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			clock := newDebounceClock(t)

			sent := clock.run(testCase.events, 40)
			if len(sent) != len(testCase.want) {
				t.Fatalf("sent %v, want %v", sent, testCase.want)
			}

			for minute, count := range testCase.want {
				if sent[minute] != count {
					t.Fatalf("sent %v, want %v", sent, testCase.want)
				}
			}
		})
	}
}

// A retried delivery keeps the schedule of its own failure: a later change must not delay it again.
func TestNotificationDebounceKeepsRetriesScheduled(t *testing.T) {
	t.Parallel()

	clock := newDebounceClock(t)
	clock.queue(0)

	clock.now = clock.started.Add(5 * time.Minute)

	deliveries, err := clock.store.Due(t.Context())
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("deliveries=%d error=%v", len(deliveries), err)
	}

	_, err = clock.store.RetryMany(t.Context(), []int64{deliveries[0].ID}, time.Minute, "SMTP unavailable")
	if err != nil {
		t.Fatal(err)
	}

	clock.queue(5*time.Minute + 30*time.Second)

	retried := clock.poll(6 * time.Minute)
	if len(retried) != 1 || retried[0].ID != deliveries[0].ID {
		t.Fatalf("retry at 6m: %v", retried)
	}

	if later := clock.poll(10 * time.Minute); len(later) != 0 {
		t.Fatalf("new change delivered before its quiet period: %v", later)
	}

	if later := clock.poll(10*time.Minute + 30*time.Second); len(later) != 1 {
		t.Fatalf("new change after its quiet period: %v", later)
	}
}
