package catalog

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"slices"
	"time"

	"github.com/cockroachdb/pebble/v2"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

const (
	reportHoldPrefix    = "report/hold/"
	reportCleanupPrefix = "report/cleanup/"
	reportSampleSize    = 16
)

func (store *Store) reportMaintenancePending(ctx context.Context) (bool, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	if !store.reports.Enabled {
		return false, nil
	}

	if store.state.Reports.Period.Aggregate != nil {
		return true, nil
	}

	pending := false
	err := store.iterate(ctx, reportCleanupPrefix, func(_, _ []byte) error {
		pending = true

		return errStop
	})

	return pending, withoutStop(err)
}

// ReportAggregate freezes totals independently of the bounded displayed sample.
// Cursor permits bounded, restartable aggregation without consuming the journal.
type ReportAggregate struct {
	Sample       []ReportItem `json:"sample,omitempty"`
	Counts       ReportCounts `json:"counts"`
	Observations int64        `json:"observations"`
	After        int64        `json:"after"`
	Through      int64        `json:"through"`
	Cursor       int64        `json:"cursor"`
}

type reportHold struct {
	After     int64 `json:"after"`
	Through   int64 `json:"through"`
	Remaining int   `json:"remaining"`
}

func (store *Store) setScanActive(active bool) {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	store.scanActive = active
}

func (store *Store) iterateReportRange(
	ctx context.Context,
	after, through int64,
	visit func([]byte, []byte) error,
) error {
	options := &pebble.IterOptions{}
	options.LowerBound = numberKey(reportJournalPrefix, after+1)
	options.UpperBound = numberKey(reportJournalPrefix, through+1)

	return store.iterateRange(ctx, reportJournalPrefix, options, visit)
}

func (aggregate *ReportAggregate) add(item ReportItem) {
	aggregate.Observations++
	aggregate.Counts.add(item)

	if len(aggregate.Sample) < reportSampleSize {
		// Summary samples contain paths and classification, not unbounded xattrs or commands.
		item.Detail.Fields, item.Detail.Baseline = nil, nil
		item.Detail.Actor = ""
		aggregate.Sample = append(aggregate.Sample, item)
	}
}

// Each call commits at most one bounded chunk. The next call resumes the frozen
// period; a failed commit cannot mutate the in-memory state via shared pointers.
//

func (store *Store) nextSummary(ctx context.Context, transaction *transaction, now time.Time) (*ReportDraft, error) {
	period := &transaction.state.Reports.Period
	aggregate := *period.Aggregate
	aggregate.Sample = slices.Clone(aggregate.Sample)
	period.Aggregate = &aggregate

	more, err := store.aggregateReportChunk(ctx, &aggregate)
	if err != nil {
		return nil, err
	}

	if more {
		return nil, transaction.commit()
	}

	var draft *ReportDraft

	if aggregate.Observations > 0 || store.reports.SendEmpty {
		message := reportMessage(store, *period, aggregate.Sample, false)
		message.Count = int(aggregate.Observations)
		draft = &ReportDraft{Destinations: period.Destinations, Message: message}
		transaction.put([]byte(reportDraftKey), draft)
	}

	closed := *period

	store.finishReportPeriod(transaction, now)
	carryCoverageGap(transaction, closed, draft == nil)

	return draft, transaction.commit()
}

func (store *Store) aggregateReportChunk(ctx context.Context, aggregate *ReportAggregate) (bool, error) {
	count, more := 0, false
	err := store.iterateReportRange(ctx, aggregate.Cursor, aggregate.Through, func(suffix, data []byte) error {
		if count == transactionBatchSize {
			more = true

			return errStop
		}

		if len(suffix) != numberBytes {
			return fault.New("invalid report journal key")
		}

		var item ReportItem

		decodeErr := json.Unmarshal(data, &item)
		if decodeErr != nil {
			return fault.Wrap("decode report observation", decodeErr)
		}

		aggregate.add(item)
		//nolint:gosec // Journal sequences are allocated as positive signed counters.
		aggregate.Cursor = int64(binary.BigEndian.Uint64(suffix))
		count++

		return nil
	})

	return more, withoutStop(err)
}

func (transaction *transaction) acknowledgeReportSummary(message Message) {
	if message.Report == nil || message.Report.Aggregate == nil {
		return
	}

	lookup := []byte(reportHoldPrefix + message.Report.ID)

	data, err := get(transaction.batch, lookup)
	if err != nil {
		transaction.err = err

		return
	}

	if data == nil {
		transaction.err = fault.New("summary acknowledgement has no retained evidence")

		return
	}

	var hold reportHold

	err = json.Unmarshal(data, &hold)
	if err != nil {
		transaction.err = fault.Wrap("decode summary retention", err)

		return
	}

	hold.Remaining--
	if hold.Remaining == 0 {
		transaction.remove(lookup)
		transaction.put([]byte(reportCleanupPrefix+message.Report.ID), hold)
	} else {
		transaction.put(lookup, hold)
	}
}

func (store *Store) cleanReportEvidence(ctx context.Context) error {
	var (
		hold   reportHold
		lookup []byte
	)

	err := store.iterate(ctx, reportCleanupPrefix, func(suffix, data []byte) error {
		lookup = key(reportCleanupPrefix, suffix)

		decodeErr := json.Unmarshal(data, &hold)
		if decodeErr != nil {
			return fault.Wrap("decode report cleanup", decodeErr)
		}

		return errStop
	})
	if withoutStop(err) != nil || lookup == nil {
		return withoutStop(err)
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	count, more := 0, false

	err = store.iterateReportRange(ctx, hold.After, hold.Through, func(suffix, _ []byte) error {
		if count == transactionBatchSize {
			more = true

			return errStop
		}

		transaction.remove(key(reportJournalPrefix, suffix))

		if len(suffix) != numberBytes {
			return fault.New("invalid report cleanup sequence")
		}
		//nolint:gosec // Journal sequences are allocated as positive signed counters.
		hold.After = int64(binary.BigEndian.Uint64(suffix))
		count++

		return nil
	})
	if withoutStop(err) != nil {
		return withoutStop(err)
	}

	transaction.state.Reports.Pending -= int64(count)
	if !more {
		transaction.remove(lookup)
	} else {
		transaction.put(lookup, hold)
	}

	return transaction.commit()
}

func (store *Store) previewSummary(ctx context.Context, period reportPeriod) (Message, error) {
	aggregate := ReportAggregate{
		After: period.After, Through: period.Through, Cursor: period.After,
		Counts: ReportCounts{}, Sample: nil, Observations: 0,
	}
	for {
		more, err := store.aggregateReportChunk(ctx, &aggregate)
		if err != nil {
			return Message{}, err
		}

		if !more {
			break
		}
	}

	period.Aggregate = &aggregate
	message := reportMessage(store, period, aggregate.Sample, false)
	message.Count = int(aggregate.Observations)

	return message, nil
}
