package catalog

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/resource"
)

// The accepted prefix of a multipart message must survive a failed attempt and a restart, without
// rescheduling the deliveries or counting a failed attempt by itself.
func TestProgressManySurvivesRetryAndRestart(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), config.CatalogDirectory)
	store := testStoreAt(t, path)
	store.Notifications.Use = []string{fixtureDestinationFirst}

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

	identifiers := []int64{deliveries[0].ID, deliveries[1].ID}
	progress := Progress{Fingerprint: "rendering", Sent: 12000}

	err = store.ProgressMany(t.Context(), identifiers, progress)
	if err != nil {
		t.Fatal(err)
	}

	assertProgress(t, store, progress, 0)

	_, err = store.RetryMany(t.Context(), identifiers, time.Second, "Slack rate limit")
	if err != nil {
		t.Fatal(err)
	}

	resource.Close(store)
	store.database = nil

	reopened := testStoreAt(t, path)
	reopened.now = func() time.Time { return time.Now().Add(time.Hour) }

	assertProgress(t, reopened, progress, 1)
}

func assertProgress(t *testing.T, store *Store, progress Progress, attempts int) {
	t.Helper()

	due, err := store.Due(t.Context())
	if err != nil || len(due) != 2 {
		t.Fatalf("due=%d error=%v", len(due), err)
	}

	for _, delivery := range due {
		if delivery.Progress != progress || delivery.Attempts != attempts {
			t.Fatalf("progress=%+v attempts=%d", delivery.Progress, delivery.Attempts)
		}
	}
}
