package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/inode64/fsledger/internal/resource"
)

func TestCompleteIntentsPreservesUnselectedOperations(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "intents.pebble")
	store := testStoreAt(t, path)

	identifiers := make([]string, transactionBatchSize+1)
	for index := range identifiers {
		identifiers[index] = strconv.Itoa(index)

		err := store.Intent(t.Context(), identifiers[index])
		if err != nil {
			t.Fatal(err)
		}
	}

	err := store.Intent(t.Context(), "unselected")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err = store.CompleteIntents(ctx, identifiers)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled cleanup succeeded", err)
	}

	pending, err := store.PendingIntents(t.Context())
	if err != nil || len(pending) != len(identifiers)+1 {
		t.Fatal("cancelled cleanup removed operations", pending, err)
	}

	err = store.CompleteIntents(t.Context(), identifiers)
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

	pending, err = reopened.PendingIntents(t.Context())
	if err != nil || !slices.Equal(pending, []string{"unselected"}) {
		t.Fatal("recovery lost an unselected operation or retained completed ones", pending, err)
	}
}
