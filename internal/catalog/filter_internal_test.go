package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/filter"
	"github.com/inode64/fsledger/internal/resource"
)

//nolint:cyclop,gocyclo,funlen // Ordered assertions cover filtering without losing integrity evidence.
func TestIgnoreOnlySuppressesChangeNotIntegrityOrState(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	record := workloadRecord(0)
	reference(t, store, record)

	before, err := store.Due(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	for _, delivery := range before {
		err = store.DeliveredMany(t.Context(), []int64{delivery.ID})
		if err != nil {
			t.Fatal(err)
		}
	}

	store.ignore, err = filter.Compile(
		[]filter.Rule{{Users: []string{"uid:123"}, CommandRegex: []string{"cron"}, Paths: nil}},
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx := filter.WithActors(t.Context(), func(string) event.Actor {
		return event.Actor{Known: true, UserKnown: true, UID: 123, Command: "php cron"}
	})
	record.Mode = 0o700

	_, _, err = store.apply(
		ctx,
		[]mutation{{refreshHash: false, excluded: false, record: record, previous: nil, deleted: false}},
		0,
		"filtered",
		"actor",
	)
	if err != nil {
		t.Fatal(err)
	}

	messages, err := store.Due(t.Context())
	if err != nil || len(messages) != 2 {
		t.Fatal("integrity alerts lost", len(messages), err)
	}

	for _, delivery := range messages {
		if delivery.Message.Event != "integrity_violation" {
			t.Fatal("ignored change enqueued")
		}
	}

	current, exists, err := store.Current(t.Context(), record.Path)
	if err != nil || !exists || current.Mode != record.Mode || len(pendingIDs(t, store)) != 1 {
		t.Fatal("filter suppressed inventory or baseline difference", err)
	}

	record.Mode = 0o600
	observeRecords(t, store, fixtureActorUnknown, record)

	messages, err = store.Due(t.Context())
	if err != nil || len(messages) != 4 {
		t.Fatal("unknown actor was suppressed", len(messages), err)
	}
}

func TestDeferredVersionsSurviveReopenAndCancellation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "catalog.pebble")
	store := testStoreAt(t, path)
	original := map[string]Deferred{"/binary-\xff\nfile": {Signature: "blob", Policy: "policy", Since: 123}}

	err := store.SaveDeferred(t.Context(), original)
	if err != nil {
		t.Fatal(err)
	}

	policy, notifications := store.Policy, store.Notifications
	resource.Close(store)
	store.database = nil

	reopened, err := Open(t.Context(), path, "test", "host", policy, notifications)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(reopened)

	pending, err := reopened.Deferred(t.Context())
	if err != nil || pending["/binary-\xff\nfile"] != original["/binary-\xff\nfile"] {
		t.Fatal("lost deferred state", pending, err)
	}

	stats, err := reopened.Stats(t.Context())
	if err != nil || stats.DeferredPaths != 1 || stats.DeferredSince != 123 {
		t.Fatal("lost deferred counters", stats, err)
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	err = reopened.SaveDeferred(cancelled, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled mutation accepted", err)
	}

	stats, err = reopened.Stats(t.Context())
	if err != nil || stats.DeferredPaths != 1 {
		t.Fatal("cancelled mutation lost pending", err)
	}

	err = reopened.SaveDeferred(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	stats, err = reopened.Stats(t.Context())
	if err != nil || stats.DeferredPaths != 0 || stats.DeferredSince != 0 {
		t.Fatal("cleanup left counters", stats, err)
	}
}

func TestExcludedObservationRetiresTrackingWithoutDeletionAlert(t *testing.T) {
	t.Parallel()
	store, scanner, matcher, root := subtreeFixture(t)
	path := filepath.Join(root, "a", "kept")

	before, err := store.Due(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Reconcile(t.Context(), scanner, []string{root}, matcher.Protect(path), false, "periodic", "")
	if err != nil {
		t.Fatal(err)
	}

	_, exists, err := store.Current(t.Context(), []byte(path))
	if err != nil || exists {
		t.Fatal("excluded path still tracked", err)
	}

	after, err := store.Due(t.Context())
	if err != nil || len(after) != len(before) || len(pendingIDs(t, store)) != 0 {
		t.Fatal("exclusion reported source deletion", err)
	}

	record, err := scanner.Observe(t.Context(), path, "sha256", true)
	if err != nil || record.Type != "regular" {
		t.Fatal("source was removed", err)
	}
}
