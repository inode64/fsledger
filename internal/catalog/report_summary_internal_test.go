package catalog

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/resource"
)

func summaryFixture(t *testing.T, store *Store, count int) *time.Time {
	t.Helper()
	now := configureReportClock(t, store)
	settings := reportSettings()
	settings.Format = "summary"
	settings.Use = []string{fixtureDestinationFirst, fixtureDestinationOther}

	err := store.ConfigureReports(t.Context(), settings, false)
	if err != nil {
		t.Fatal(err)
	}

	store.Notifications.Use = nil
	for index := range count {
		observeRecords(t, store, "bulk", workloadRecord(index))
	}

	*now = now.Add(time.Minute)

	return now
}

func assertReportJournal(t *testing.T, store *Store, want int) {
	t.Helper()

	count := 0

	err := store.iterate(t.Context(), reportJournalPrefix, func(_, _ []byte) error {
		count++

		return nil
	})
	if err != nil || count != want || store.Reports().Pending != int64(want) {
		t.Fatalf("retained evidence=%d pending=%d want=%d error=%v", count, store.Reports().Pending, want, err)
	}
}

func TestSummaryRetainsAllEvidenceUntilEveryDestinationAcknowledges(t *testing.T) {
	t.Parallel()
	store := testStore(t)

	const count = 1100

	now := summaryFixture(t, store, count)

	preview, err := store.PreviewReport(t.Context())
	if err != nil || preview.Report.Aggregate.Observations != count || len(preview.Report.Items) != reportSampleSize {
		t.Fatal("preview did not summarize complete period", err)
	}

	assertReportJournal(t, store, count)

	draft := nextReport(t, store)
	if draft.Message.Count != count || draft.Message.Report.Counts().Added != count || draft.Message.Report.More {
		t.Fatal("summary emitted parts or incorrect totals", draft)
	}

	publishReport(t, store, draft)

	deliveries, err := store.Due(t.Context())
	if err != nil || len(deliveries) != 2 {
		t.Fatal("expected one summary per destination", deliveries, err)
	}

	err = store.RetryMany(t.Context(), []int64{deliveries[0].ID}, time.Second, "SMTP unavailable")
	if err != nil {
		t.Fatal(err)
	}

	err = store.DeliveredMany(t.Context(), []int64{deliveries[1].ID})
	if err != nil {
		t.Fatal(err)
	}

	assertReportJournal(t, store, count)

	err = store.DeliveredMany(t.Context(), []int64{deliveries[0].ID})
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.NextReport(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	assertReportJournal(t, store, 0)

	*now = now.Add(time.Minute)

	empty, err := store.NextReport(t.Context())
	if err != nil || empty != nil {
		t.Fatal("empty period emitted a summary", err)
	}
}

func TestSummaryAggregationResumesAfterRestartAndFreezesPeriod(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), config.CatalogDirectory)
	store := testStoreAt(t, path)

	const count = 700

	now := summaryFixture(t, store, count)

	_, err := store.nextReportStep(t.Context())
	if err != nil || store.state.Reports.Period.Aggregate.Observations != transactionBatchSize {
		t.Fatal("aggregation did not persist one bounded chunk", err)
	}

	observeRecords(t, store, "later period", workloadRecord(count))
	policy, notifications, reports := store.Policy, store.Notifications, store.reports
	resource.Close(store)
	store.database = nil

	reopened, err := Open(t.Context(), path, "test", "host", policy, notifications)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(reopened)

	reopened.now = func() time.Time { return *now }

	err = reopened.ConfigureReports(t.Context(), reports, true)
	if err != nil {
		t.Fatal(err)
	}

	draft := nextReport(t, reopened)
	if draft.Message.Count != count || draft.Message.Report.Counts().Added != count {
		t.Fatal("resumption double-counted or included a later observation", draft)
	}

	publishReport(t, reopened, draft)

	*now = now.Add(time.Minute)

	next := nextReport(t, reopened)
	if next.Message.Count != 1 || next.Message.Report.Counts().Added != 1 {
		t.Fatal("unacknowledged previous summary contaminated the next period", next)
	}

	assertReportJournal(t, reopened, count+1)
}

func TestSummaryCountsViolationsAndMarksActiveScan(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	_ = summaryFixture(t, store, 1)
	store.setScanActive(true)
	transaction := store.begin()

	detail := Detail{}
	detail.Path, detail.Kind = []byte("/changed"), changeModified

	err := store.recordReport(transaction, detail, true)
	if err == nil {
		err = transaction.commit()
	}

	resource.Close(transaction.batch)

	if err != nil {
		t.Fatal(err)
	}

	draft := nextReport(t, store)
	if !draft.Message.Report.CoverageGap || draft.Message.Report.Counts().Violations != 1 {
		t.Fatal("summary hid incomplete scan or baseline violation", draft)
	}
}

// A restart marks a coverage gap; with send_empty false and nothing observed no summary is sent,
// and the next summary with observations still carries the warning.
func TestSummaryEmptyGapDeferredToNextPeriod(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	now := summaryFixture(t, store, 0)

	settings := reportSettings()
	settings.Format = "summary"

	err := store.ConfigureReports(t.Context(), settings, true)
	if err != nil {
		t.Fatal(err)
	}

	empty, err := store.NextReport(t.Context())
	if err != nil || empty != nil {
		t.Fatal("empty summary sent after restart", empty, err)
	}

	if !store.Reports().CoverageGap {
		t.Fatal("restart coverage gap dropped with the empty period")
	}

	observeRecords(t, store, "after-restart", workloadRecord(0))

	*now = now.Add(time.Minute)

	draft := nextReport(t, store)
	if !draft.Message.Report.CoverageGap || draft.Message.Report.Aggregate.Observations != 1 {
		t.Fatal("carried coverage gap missing from the next summary", draft)
	}
}
