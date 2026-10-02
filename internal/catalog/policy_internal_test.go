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

	for _, field := range []string{"comparison", "algorithm"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			store := testStore(t)
			record := integrity.Record{
				Path: []byte("/policy"), Type: fixtureRegular, Algorithm: fixtureSHA256, Hash: strings.Repeat("a", 64),
			}
			reference(t, store, record)
			record.Hash = strings.Repeat("b", 64)
			observeRecords(t, store, "policy-change", record)

			original := store.Policy

			original.Compare = slices.Clone(original.Compare)
			if field == "comparison" {
				// In-place edits must not also mutate the cached comparison fields.
				store.Policy.Compare[0] = "size"
			} else {
				store.Policy.Hash.Algorithm = config.HashSHA512
			}

			err := store.Accept(t.Context(), "policy-change")
			if err == nil || len(pendingIDs(t, store)) != 1 {
				t.Fatal("obsolete policy approved a pending change", err)
			}

			store.Policy = original

			err = store.Accept(t.Context(), "policy-change")
			if err != nil || len(pendingIDs(t, store)) != 0 {
				t.Fatal("restored policy could not approve the unchanged pending version", err)
			}
		})
	}
}
