package catalog

import (
	"math"
	"slices"
	"strconv"
	"testing"

	"github.com/inode64/fsledger/internal/resource"
)

func TestReportItemsHonorRangeBounds(t *testing.T) {
	t.Parallel()
	store := testStore(t)

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	for _, sequence := range []int64{1, 2, 3, math.MaxInt64, math.MinInt64} {
		transaction.put(numberKey(reportJournalPrefix, sequence), ReportItem{
			Detail: Detail{
				ChangeID: strconv.FormatInt(sequence, 10), Kind: "", Actor: "", Path: nil,
				Fields: nil, Baseline: nil, Observed: 0,
			},
			Head: "", Deferred: false, Violation: false,
		})
	}

	err := transaction.commit()
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name           string
		want           []string
		after, through int64
	}{
		{"upper inclusive", []string{"1", "2"}, 0, 2},
		{"lower exclusive", []string{"2", "3"}, 1, 3},
		{"empty", nil, 2, 2},
		{"negative minus one", nil, 0, -1},
		{"negative wrapping upper bound", nil, 0, -2},
		{"maximum sequence", []string{strconv.FormatInt(math.MaxInt64, 10)}, 3, math.MaxInt64},
		{"maximum empty", nil, math.MaxInt64, math.MaxInt64},
	} {
		items, keys, more, readErr := store.reportItems(t.Context(), test.after, test.through)
		if readErr != nil || more || len(keys) != len(test.want) {
			t.Fatal(test.name, "unexpected range result", len(keys), more, readErr)
		}

		got := make([]string, 0, len(items))
		for _, item := range items {
			got = append(got, item.Detail.ChangeID)
		}

		if !slices.Equal(got, test.want) {
			t.Fatal(test.name, "read events outside the requested range", got)
		}
	}
}
