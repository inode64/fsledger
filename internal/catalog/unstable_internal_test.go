package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/integrity"
)

const fullScanOperation = "full"

//nolint:cyclop,funlen,gocognit,gocyclo // Verify the same partial-result contract through each scan entry point.
func TestUnstableInventoryPreservesRecordsAndProcessesStablePaths(t *testing.T) {
	t.Parallel()

	for _, operation := range []string{fullScanOperation, "incremental", "events", "directory"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			store, scanner, matcher, root := subtreeFixture(t)
			hot, stable, deleted := filepath.Join(
				root,
				"a",
				"kept",
			), filepath.Join(
				root,
				"b",
				"file",
			), filepath.Join(
				root,
				"a",
				"gone",
			)

			previous, _, err := store.Current(t.Context(), []byte(hot))
			if err != nil {
				t.Fatal(err)
			}

			lastFull := store.state.Stats.LastFullScan

			for _, path := range []string{hot, stable} {
				err = os.WriteFile(path, []byte("changed"), 0o600)
				if err != nil {
					t.Fatal(err)
				}
			}

			err = os.Remove(deleted)
			if err != nil {
				t.Fatal(err)
			}

			mutations := 0

			var mutationErr error

			store.BindCopiedHashes(func(record integrity.Record, _ string) (string, int64, bool) {
				if string(record.Path) == hot {
					mutations++
					mutationErr = os.WriteFile(hot, make([]byte, mutations), 0o600)
				}

				return "", 0, false
			})

			switch operation {
			case fullScanOperation, "incremental":
				_, err = store.Reconcile(
					t.Context(),
					scanner,
					[]string{root},
					matcher,
					operation == fullScanOperation,
					"unknown",
					"",
				)
			case "events":
				_, err = store.Observe(
					t.Context(),
					scanner,
					[]string{hot, stable, deleted},
					[]string{root},
					matcher,
					"actor",
					"",
				)
			case "directory":
				_, err = store.Observe(t.Context(), scanner, []string{root}, []string{root}, matcher, "actor", "")
			}

			paths, fatal := integrity.SplitUnstable(err)
			if fatal != nil || len(paths) != 1 || paths[0] != hot || mutations != 3 || mutationErr != nil {
				t.Fatal(paths, fatal, mutations, mutationErr)
			}

			current, exists, err := store.Current(t.Context(), []byte(hot))
			if err != nil || !exists || current.Hash != previous.Hash || current.Observed != previous.Observed {
				t.Fatal("unstable record changed or was pruned", current, err)
			}

			_, exists, err = store.Current(t.Context(), []byte(deleted))
			if err != nil || exists {
				t.Fatal("stable deletion blocked", err)
			}

			if pendingIDs(t, store)[stable] == "" || store.state.Complete ||
				store.state.Stats.LastFullScan != lastFull {
				t.Fatal("stable update lost or partial inventory certified")
			}

			store.BindCopiedHashes(nil)

			_, err = store.Reconcile(t.Context(), scanner, []string{root}, matcher, true, "retry", "")
			if err != nil || !store.state.Complete || pendingIDs(t, store)[hot] == "" {
				t.Fatal("stable retry not recovered", err)
			}
		})
	}
}

func TestPartialScanPreservesUnstableFormerDirectory(t *testing.T) {
	t.Parallel()
	store, scanner, matcher, root := subtreeFixture(t)
	hot := filepath.Join(root, "a")

	err := os.RemoveAll(hot)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(hot, []byte("new file"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mutations := 0

	var mutationErr error

	store.BindCopiedHashes(func(record integrity.Record, _ string) (string, int64, bool) {
		if string(record.Path) == hot {
			mutations++
			mutationErr = os.WriteFile(hot, make([]byte, mutations), 0o600)
		}

		return "", 0, false
	})

	_, err = store.Reconcile(t.Context(), scanner, []string{root}, matcher, true, "partial", "")
	if !errors.Is(err, integrity.ErrUnstable) || mutationErr != nil {
		t.Fatal(err, mutationErr)
	}

	for _, path := range []string{hot, filepath.Join(hot, "kept"), filepath.Join(hot, "gone")} {
		_, exists, currentErr := store.Current(t.Context(), []byte(path))
		if currentErr != nil || !exists {
			t.Fatal("previous subtree pruned", path, currentErr)
		}
	}
}
