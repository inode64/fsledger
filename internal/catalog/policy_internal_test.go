package catalog

import (
	"slices"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/integrity"
)

func TestApprovalRejectsChangedPolicy(t *testing.T) {
	t.Parallel()

	for _, field := range []string{"comparison", "algorithm", "directories"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			path := t.TempDir()
			store := testStoreAt(t, path)
			record := integrity.Record{
				Path: []byte("/policy"), Type: fixtureRegular, Algorithm: fixtureSHA256, Hash: strings.Repeat("a", 64),
			}
			reference(t, store, record)
			record.Hash = strings.Repeat("b", 64)
			observeRecords(t, store, "policy-change", record)

			original := store.policy
			changed := original
			changed.Compare = slices.Clone(original.Compare)

			switch field {
			case "comparison":
				changed.Compare[0] = "size"
			case "algorithm":
				changed.Hash.Algorithm = config.HashSHA512
			case "directories":
				changed.Directories = !original.Directories
			}

			store = reopenTestStore(t, store, path, changed)

			err := store.Accept(t.Context(), "policy-change")
			if err == nil || len(pendingIDs(t, store)) != 1 {
				t.Fatal("obsolete policy approved a pending change", err)
			}

			store = reopenTestStore(t, store, path, original)

			err = store.Accept(t.Context(), "policy-change")
			if err != nil || len(pendingIDs(t, store)) != 0 {
				t.Fatal("restored policy could not approve the unchanged pending version", err)
			}
		})
	}
}

func reopenTestStore(t *testing.T, store *Store, path string, policy config.Integrity) *Store {
	t.Helper()

	err := store.Close()
	if err != nil {
		t.Fatal(err)
	}

	store.database = nil

	return openTestStore(t, path, policy, store.Notifications)
}

func TestOpenOwnsIntegrityPolicy(t *testing.T) {
	t.Parallel()
	cfg := testStoreConfig(t)
	store := openTestStore(t, t.TempDir(), cfg.Integrity, cfg.Notifications)
	record := workloadRecord(0)
	reference(t, store, record)
	// Editing the caller's configuration must not disable hash comparison or change its algorithm.
	cfg.Integrity.Compare[0] = fieldSize
	cfg.Integrity.Hash.Algorithm = config.HashSHA512
	record.Hash = strings.Repeat("b", 64)
	observeRecords(t, store, "owned-policy", record)

	if len(pendingIDs(t, store)) != 1 {
		t.Fatal("caller changed the open store's comparison policy")
	}

	err := store.Accept(t.Context(), "owned-policy")
	if err != nil || len(pendingIDs(t, store)) != 0 {
		t.Fatal("caller changed the open store's approval policy", err)
	}
}
