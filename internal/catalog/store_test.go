package catalog_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/resource"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/integrity"
)

func options(t *testing.T) (config.Integrity, config.Notifications) {
	t.Helper()

	cfg, err := config.Load(testConfiguration(t))
	if err != nil {
		t.Fatal(err)
	}

	cfg.Integrity.Compare = []string{"hash", "type", "mode", "uid", "gid", "xattrs", "acl"}
	cfg.Notifications.Use = []string{"test"}
	cfg.Notifications.BatchWindow = 0

	return cfg.Integrity, cfg.Notifications
}

//nolint:cyclop,funlen,gocognit,gocyclo // Ordered integration fixture keeps setup, transitions and assertions together.
func TestBaselinePendingAndOutbox(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	source := filepath.Join(root, "source")

	err := os.Mkdir(source, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(source, "file")

	err = os.WriteFile(file, []byte("before"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	policy, notifications := options(t)
	path := filepath.Join(root, "db", "catalog.pebble")

	store, err := catalog.Open(t.Context(), path, "test", "host", policy, notifications)
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		if store != nil {
			resource.Close(store)
		}
	}()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	scanner := integrity.NewScanner(2, 2)
	scan := func() catalog.Result {
		t.Helper()

		result, scanErr := store.Reconcile(t.Context(), scanner, []string{source}, matcher, true, "unknown", "")
		if scanErr != nil {
			t.Fatal(scanErr)
		}

		return result
	}
	scan()

	err = store.InitBaseline(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(file, []byte("after!"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	change := scan()

	stats, err := store.Stats(t.Context())
	if err != nil || stats.Violations != 1 || change.Changed != 1 {
		t.Fatalf("%+v %+v %v", stats, change, err)
	}

	if repeat := scan(); repeat.Changed != 0 || repeat.Violations != 0 {
		t.Fatalf("duplicate change: %+v", repeat)
	}

	err = store.Close()
	if err != nil {
		t.Fatal(err)
	}

	store, err = catalog.Open(t.Context(), path, "test", "host", policy, notifications)
	if err != nil {
		t.Fatal(err)
	}

	due, err := store.Due(t.Context())
	if err != nil || len(due) == 0 {
		t.Fatalf("outbox not durable: %v", err)
	}

	err = store.Accept(t.Context(), change.ID)
	if err != nil {
		t.Fatal(err)
	}

	scan()

	stats, err = store.Stats(t.Context())
	if err != nil || stats.Violations != 0 {
		t.Fatalf("approval failed %+v %v", stats, err)
	}

	err = os.Remove(file)
	if err != nil {
		t.Fatal(err)
	}

	deleted := scan()

	stats, err = store.Stats(t.Context())
	if err != nil || stats.Entries != 1 || stats.Violations != 1 {
		t.Fatalf("delete %+v %v", stats, err)
	}

	err = store.Accept(t.Context(), deleted.ID)
	if err != nil {
		t.Fatal(err)
	}

	scan()

	stats, err = store.Stats(t.Context())
	if err != nil || stats.Violations != 0 {
		t.Fatalf("delete approval %+v %v", stats, err)
	}
}

//nolint:funlen // The partial-scan lifecycle keeps preservation and approval assertions together.
func TestIncompleteScanNeverPrunes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	source := filepath.Join(root, "source")

	err := os.Mkdir(source, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(source, "file"), []byte("keep"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	policy, notifications := options(t)

	store, err := catalog.Open(
		t.Context(),
		filepath.Join(root, "db", "catalog.pebble"),
		"test",
		"host",
		policy,
		notifications,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(store)

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	scanner := integrity.NewScanner(1, 2)

	_, err = store.Reconcile(t.Context(), scanner, []string{source}, matcher, true, "unknown", "")
	if err != nil {
		t.Fatal(err)
	}

	err = os.Rename(source, source+"-offline")
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Reconcile(t.Context(), scanner, []string{source}, matcher, true, "unknown", "")
	if err == nil {
		t.Fatal("missing source accepted")
	}

	stats, err := store.Stats(t.Context())
	if err != nil || stats.Entries != 2 {
		t.Fatalf("partial scan pruned: %+v %v", stats, err)
	}

	err = store.InitBaseline(t.Context())
	if err == nil {
		t.Fatal("partial baseline approved")
	}
}

func TestRetryAndQueueOverload(t *testing.T) {
	t.Parallel()
	policy, notifications := options(t)
	notifications.MaxPending = 1

	store, err := catalog.Open(
		t.Context(),
		filepath.Join(t.TempDir(), "catalog.pebble"),
		"test",
		"host",
		policy,
		notifications,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(store)

	err = store.Announce(t.Context(), "error", "")
	if err != nil {
		t.Fatal(err)
	}

	err = store.Announce(t.Context(), "error", "")
	if err != nil {
		t.Fatal("overload blocked repository", err)
	}

	due, err := store.Due(t.Context())
	if err != nil || len(due) != 2 {
		t.Fatalf("%v %v", due, err)
	}

	err = store.RetryMany(t.Context(), []int64{due[0].ID}, time.Hour, "temporary failure")
	if err != nil {
		t.Fatal(err)
	}

	err = store.DeliveredMany(t.Context(), []int64{due[1].ID})
	if err != nil {
		t.Fatal(err)
	}

	ready, queryErr := store.Due(t.Context())
	if queryErr != nil || len(ready) != 0 {
		t.Fatal("backoff ignored")
	}

	err = store.DeliveredMany(context.Background(), []int64{due[0].ID})
	if err != nil {
		t.Fatal(err)
	}
}
