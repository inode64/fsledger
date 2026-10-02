package catalog

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/filter"
	"github.com/inode64/fsledger/internal/resource"
)

func reportSettings() config.Reports {
	return config.Reports{
		Format:  "detailed",
		Enabled: true, Schedule: "* * * * *", Timezone: "UTC",
		Use: []string{fixtureDestinationFirst}, SendEmpty: false,
		AI: config.ReportAI{Enabled: false, Input: "metadata", Profiles: nil, Include: nil},
	}
}

func TestReportIgnoreDisabledAlertsAndDeferredMetadata(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	now := configureReportClock(t, store)
	record := workloadRecord(0)

	var err error

	store.ignore, err = filter.Compile(
		[]filter.Rule{{Paths: []string{string(record.Path)}, Users: nil, CommandRegex: nil}},
	)
	if err != nil {
		t.Fatal(err)
	}

	observeRecords(t, store, "ignored", record)

	if store.Reports().Pending != 1 {
		t.Fatal("ignored alert suppressed report evidence")
	}

	deliveries, err := store.Due(t.Context())
	if err != nil || len(deliveries) != 0 {
		t.Fatal("ignored alert queued", deliveries, err)
	}

	store.Notifications.Use, store.Notifications.Events = nil, nil

	err = store.SaveDeferred(
		t.Context(),
		map[string]Deferred{string(record.Path): {Signature: "known", Policy: "report-defer-policy", Since: 1}},
	)
	if err != nil {
		t.Fatal(err)
	}

	store.ReportHead("known-head")

	record.Mode = 0o700
	observeRecords(t, store, "retained", record)

	*now = now.Add(time.Minute)

	draft := nextReport(t, store)
	if len(draft.Message.Report.Items) != 2 || draft.Message.Report.Items[0].Detail.ChangeID != "ignored" {
		t.Fatal("report depends on immediate alerts")
	}

	item := draft.Message.Report.Items[1]
	if !item.Deferred || item.Head != "known-head" || item.Detail.ChangeID != "retained" {
		t.Fatal(item)
	}
}

func TestReportDisableAndCancellationRetainEvidence(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	now := configureReportClock(t, store)
	observeRecords(t, store, "retained", workloadRecord(0))

	settings := reportSettings()
	settings.Enabled = false

	err := store.ConfigureReports(t.Context(), settings, false)
	if err != nil {
		t.Fatal(err)
	}

	draft, err := store.NextReport(t.Context())
	if err != nil || draft != nil || store.Reports().Pending != 1 {
		t.Fatal("disable lost evidence", err)
	}

	settings.Enabled = true

	err = store.ConfigureReports(t.Context(), settings, false)
	if err != nil {
		t.Fatal(err)
	}

	*now = now.Add(time.Minute)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = store.NextReport(cancelled)
	if err == nil || store.Reports().Pending != 1 {
		t.Fatal("cancelled cut lost evidence", err)
	}

	draft = nextReport(t, store)
	if !draft.Message.Report.CoverageGap {
		t.Fatal("disabled period not marked incomplete")
	}

	err = store.PublishReport(cancelled, draft.Message.ChangeID, "", "disabled")
	if err == nil {
		t.Fatal("cancelled publish succeeded")
	}

	if actual := nextReport(t, store); !reflect.DeepEqual(actual, draft) {
		t.Fatal("cancelled publish changed draft")
	}
}

func configureReportClock(t *testing.T, store *Store) *time.Time {
	t.Helper()

	now := time.Date(2026, time.September, 20, 0, 0, 0, 0, time.UTC)

	store.now = func() time.Time { return now }

	err := store.ConfigureReports(t.Context(), reportSettings(), false)
	if err != nil {
		t.Fatal(err)
	}

	return &now
}

func nextReport(t *testing.T, store *Store) *ReportDraft {
	t.Helper()

	draft, err := store.NextReport(t.Context())
	if err != nil || draft == nil {
		t.Fatalf("missing report: %v", err)
	}

	return draft
}

func publishReport(t *testing.T, store *Store, draft *ReportDraft) {
	t.Helper()

	err := store.PublishReport(t.Context(), draft.Message.ChangeID, "frozen summary", "disabled")
	if err != nil {
		t.Fatal(err)
	}
}

//nolint:cyclop,gocyclo,funlen // One lifecycle verifies alert acknowledgement, crash recovery and SMTP retry identity.
func TestReportSurvivesAlertAcknowledgementAndRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), config.CatalogDirectory)
	store := testStoreAt(t, path)
	now := configureReportClock(t, store)
	observeRecords(t, store, "observed", workloadRecord(0))

	deliveries, err := store.Due(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	for _, delivery := range deliveries {
		err = store.DeliveredMany(t.Context(), []int64{delivery.ID})
		if err != nil {
			t.Fatal(err)
		}
	}

	*now = now.Add(time.Minute)

	draft := nextReport(t, store)
	if len(draft.Message.Report.Items) != 1 || draft.Message.Report.Items[0].Detail.ChangeID != "observed" {
		t.Fatal("alert acknowledgement lost evidence", draft)
	}

	policy, notifications := store.Policy, store.Notifications
	resource.Close(store)
	store.database = nil

	reopened, err := Open(t.Context(), path, "test", "host", policy, notifications)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(reopened)

	reopened.now = func() time.Time { return *now }

	err = reopened.ConfigureReports(t.Context(), reportSettings(), true)
	if err != nil {
		t.Fatal(err)
	}

	if actual := nextReport(t, reopened); !reflect.DeepEqual(actual, draft) {
		t.Fatal("draft changed after restart")
	}

	publishReport(t, reopened, draft)

	deliveries, err = reopened.Due(t.Context())
	if err != nil || len(deliveries) != 1 || deliveries[0].Message.Report.Summary != "frozen summary" {
		t.Fatal("report not queued", deliveries, err)
	}

	err = reopened.RetryMany(t.Context(), []int64{deliveries[0].ID}, time.Second, "SMTP unavailable")
	if err != nil {
		t.Fatal(err)
	}

	*now = now.Add(time.Second)

	retried, err := reopened.Due(t.Context())
	if err != nil || len(retried) != 1 || retried[0].Message.ChangeID != draft.Message.ChangeID {
		t.Fatal(retried, err)
	}

	if !reopened.Reports().CoverageGap {
		t.Fatal("restart coverage gap missing")
	}
}

func TestReportPartsFreezeCutAndRouting(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	store.Notifications.Use = nil
	now := configureReportClock(t, store)

	const count = 130
	for index := range count {
		observeRecords(t, store, "bulk", workloadRecord(index))
	}

	*now = now.Add(time.Minute)
	draft := nextReport(t, store)
	periodID := draft.Message.Report.ID

	seen := len(draft.Message.Report.Items)
	if !draft.Message.Report.More {
		t.Fatal("expected bounded parts")
	}

	publishReport(t, store, draft)
	observeRecords(t, store, "late", workloadRecord(count))

	settings := reportSettings()

	settings.Use = []string{fixtureDestinationOther}

	err := store.ConfigureReports(t.Context(), settings, false)
	if err != nil {
		t.Fatal(err)
	}

	for part := 2; draft.Message.Report.More; part++ {
		draft = nextReport(t, store)
		if draft.Message.Report.ID != periodID || draft.Message.Report.Part != part || len(draft.Destinations) != 1 {
			t.Fatal("inconsistent report part", draft)
		}

		if _, exists := draft.Destinations[fixtureDestinationFirst]; !exists {
			t.Fatal("routing changed mid-period")
		}

		seen += len(draft.Message.Report.Items)
		publishReport(t, store, draft)
	}

	if seen != count || store.Reports().Pending != 1 {
		t.Fatal("lost, duplicated or misassigned observations", seen, store.Reports())
	}

	*now = now.Add(time.Minute)

	draft = nextReport(t, store)
	if len(draft.Message.Report.Items) != 1 || draft.Message.Report.Items[0].Detail.ChangeID != "late" {
		t.Fatal("late observation not retained")
	}
}

func TestReportPreviewEmptyAndCoverage(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	now := configureReportClock(t, store)

	before := store.Reports()

	_, err := store.PreviewReport(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if store.Reports() != before {
		t.Fatal("preview advanced cursor")
	}

	*now = now.Add(time.Minute)

	empty, err := store.NextReport(t.Context())
	if err != nil || empty != nil {
		t.Fatal("empty period sent", empty, err)
	}

	err = store.ReportGap(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	*now = now.Add(time.Minute)

	// With send_empty false a gap alone is not a report: it is carried to the next period with evidence.
	empty, err = store.NextReport(t.Context())
	if err != nil || empty != nil {
		t.Fatal("empty period with coverage gap sent", empty, err)
	}

	if !store.Reports().CoverageGap {
		t.Fatal("coverage gap dropped with the empty period")
	}

	observeRecords(t, store, "after-gap", workloadRecord(0))

	*now = now.Add(time.Minute)

	draft := nextReport(t, store)
	if !draft.Message.Report.CoverageGap || len(draft.Message.Report.Items) != 1 {
		t.Fatal("carried coverage gap missing from the next report", draft)
	}

	publishReport(t, store, draft)

	*now = now.Add(time.Minute)

	empty, err = store.NextReport(t.Context())
	if err != nil || empty != nil || store.Reports().CoverageGap {
		t.Fatal("coverage gap outlived the report that delivered it", empty, err)
	}
}

func TestReadOnlyReportDoesNotCreateCatalog(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "absent")

	_, err := ReadReport(t.Context(), path, "test", "host")
	if err == nil {
		t.Fatal("preview created a missing catalog")
	}

	store := testStoreAt(t, path)
	configureReportClock(t, store)
	observeRecords(t, store, "retained", workloadRecord(0))

	_, err = ReadReport(t.Context(), path, "test", "host")
	if err == nil {
		t.Fatal("preview acquired active writer's catalog")
	}

	resource.Close(store)
	store.database = nil

	for range 2 {
		message, readErr := ReadReport(t.Context(), path, "test", "host")
		if readErr != nil || message.Report == nil || len(message.Report.Items) != 1 {
			t.Fatal("read-only preview lost evidence", message, readErr)
		}
	}
}
