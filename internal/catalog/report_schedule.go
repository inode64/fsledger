package catalog

import (
	"context"
	"encoding/json"
	"time"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

// NextReport closes a due observation period in bounded, durable chunks. New observations
// belong to the next period even while AI generation or SMTP delivery is in progress.
//
//nolint:nilnil // A nil draft explicitly means no report is due; it is not an error.
func (store *Store) NextReport(ctx context.Context) (*ReportDraft, error) {
	for {
		draft, err := store.nextReportStep(ctx)
		if err != nil || draft != nil {
			return draft, err
		}

		pending, err := store.reportMaintenancePending(ctx)
		if err != nil {
			return nil, err
		}

		if !pending {
			return nil, nil
		}
	}
}

//nolint:nilnil // A nil draft means no report is due or aggregation needs another bounded step.
func (store *Store) nextReportStep(ctx context.Context) (*ReportDraft, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	if !store.reports.Enabled {
		return nil, nil
	}

	cleanErr := store.cleanReportEvidence(ctx)
	if cleanErr != nil {
		return nil, cleanErr
	}

	err := ctx.Err()
	if err != nil {
		return nil, fault.Wrap("prepare report", err)
	}

	draft, err := store.readReportDraft()
	if err != nil || draft != nil {
		return draft, err
	}

	now := store.now()
	if store.state.Reports.Period.ID == "" && now.Before(time.Unix(0, store.state.Reports.Next)) {
		return nil, nil
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	if transaction.state.Reports.Period.ID == "" {
		store.startReportPeriod(transaction, now)
	}

	period := transaction.state.Reports.Period
	if period.Aggregate != nil {
		return store.nextSummary(ctx, transaction, now)
	}

	return store.nextDetailedReport(ctx, transaction, now)
}

func (store *Store) nextDetailedReport(
	ctx context.Context,
	transaction *transaction,
	now time.Time,
) (*ReportDraft, error) {
	period := transaction.state.Reports.Period

	items, keys, more, err := store.reportItems(ctx, period.After, period.Through)
	if err != nil {
		return nil, err
	}

	var draft *ReportDraft
	if len(items) > 0 || store.reports.SendEmpty || period.Part > 1 {
		draft = &ReportDraft{Destinations: period.Destinations, Message: reportMessage(store, period, items, more)}
		transaction.put([]byte(reportDraftKey), draft)
	}

	for _, lookup := range keys {
		transaction.remove(lookup)
	}

	transaction.state.Reports.Pending -= int64(len(items))

	transaction.state.Reports.Period.Part++
	if !more {
		store.finishReportPeriod(transaction, now)
		carryCoverageGap(transaction, period, draft == nil)
	}

	return draft, transaction.commit()
}

func (store *Store) startReportPeriod(transaction *transaction, now time.Time) {
	state := &transaction.state.Reports

	destinations := make(map[string]string, len(store.reports.Use))
	for _, destination := range store.reports.Use {
		destinations[destination] = store.versions[destination]
	}

	state.Period = reportPeriod{
		Aggregate: nil, After: state.Through,
		ID: changeID(), Destinations: destinations, From: state.Since, Until: now.UnixNano(),
		Through: state.Sequence, Part: 1, CoverageGap: state.CoverageGap || store.scanActive,
	}
	if state.Format == "summary" {
		state.Period.Aggregate = &ReportAggregate{
			After: state.Through, Through: state.Sequence,
			Cursor: state.Through, Observations: 0, Counts: ReportCounts{}, Sample: nil,
		}
	}

	state.CoverageGap = false
}

func (store *Store) reportItems(ctx context.Context, after, through int64) ([]ReportItem, [][]byte, bool, error) {
	var (
		items []ReportItem
		keys  [][]byte
	)

	bytes, more := 0, false
	err := store.iterateReportRange(ctx, after, through, func(suffix, data []byte) error {
		if len(suffix) != numberBytes {
			return fault.New("invalid report journal key")
		}

		if through < 0 {
			return errStop
		}

		if len(items) >= notificationBatchPaths || (len(items) > 0 && bytes+len(data) > notificationBatchBytes) {
			more = true

			return errStop
		}

		var item ReportItem

		decodeErr := json.Unmarshal(data, &item)
		if decodeErr != nil {
			return fault.Wrap("decode report event", decodeErr)
		}

		items = append(items, item)
		keys = append(keys, key(reportJournalPrefix, suffix))
		bytes += len(data)

		return nil
	})

	return items, keys, more, withoutStop(err)
}

// PreviewReport reads a bounded first part without consuming evidence, calling AI or sending mail.
func (store *Store) PreviewReport(ctx context.Context) (Message, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	err := ctx.Err()
	if err != nil {
		return Message{}, fault.Wrap("preview report", err)
	}

	draft, err := store.readReportDraft()
	if err != nil {
		return Message{}, err
	}

	if draft != nil {
		return draft.Message, nil
	}

	state := store.state.Reports
	if state.Since == 0 {
		return Message{}, fault.New("reports have not recorded an observation period yet")
	}

	period := state.Period
	if period.ID == "" {
		period = reportPeriod{
			Aggregate: nil, After: state.Through,
			ID: "preview", Destinations: nil, From: state.Since, Until: store.now().UnixNano(),
			Through: state.Sequence, Part: 1, CoverageGap: state.CoverageGap,
		}
	}

	if period.Aggregate != nil || (state.Period.ID == "" && state.Format == "summary") {
		return store.previewSummary(ctx, period)
	}

	items, _, more, err := store.reportItems(ctx, period.After, period.Through)

	return reportMessage(store, period, items, more), err
}
