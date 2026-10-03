package catalog

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/integrity"
)

//nolint:funlen // The ordered regression covers durable batches and approval preservation.
func TestInterruptedReplacementRetainsPendingViolations(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	root := t.TempDir()

	const count = transactionBatchSize + 32
	for index := range count {
		err := os.WriteFile(filepath.Join(root, strconv.Itoa(index)), []byte("before"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	scanner := integrity.NewScanner(1, 1)

	_, err = store.ScanAndReplaceBaseline(t.Context(), scanner, []string{root}, matcher)
	if err != nil {
		t.Fatal(err)
	}

	for index := range count {
		err = os.WriteFile(filepath.Join(root, strconv.Itoa(index)), []byte("after"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err = store.Reconcile(t.Context(), scanner, []string{root}, matcher, true, "changed", "changes")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	observed := 0

	store.BindCopiedHashes(func(integrity.Record, string) (string, int64, bool) {
		observed++
		if observed == count {
			cancel()
		}

		return "", 0, false
	})

	_, err = store.ScanAndReplaceBaseline(ctx, scanner, []string{root}, matcher)
	if err == nil {
		t.Fatal("interrupted replacement succeeded")
	}

	if got := store.state.Stats.Violations; got != count {
		t.Fatalf("failed replacement lost pending violations: got %d, want %d", got, count)
	}

	err = store.Accept(t.Context(), "changes")
	if err != nil {
		t.Fatal("unchanged pending versions lost their approval identifier", err)
	}
}
