package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/reportclock"
	"github.com/inode64/fsledger/internal/resource"
)

const (
	reportJournalPrefix = "report/event/"
	reportDraftKey      = "report/draft"
	// ReportEvent selects the scheduled report renderer, independent of change alerts.
	ReportEvent = "report"
)

// ReportItem preserves observed metadata, not source contents or inferred identities.
type ReportItem struct {
	Head      string `json:"head,omitempty"`
	Detail    Detail `json:"detail"`
	Deferred  bool   `json:"deferred"`
	Violation bool   `json:"violation"`
}

// ReportInfo is a frozen, numbered part of an observation period.
type ReportInfo struct {
	Aggregate   *ReportAggregate `json:"aggregate,omitempty"`
	ID          string           `json:"id"`
	Summary     string           `json:"summary"`
	AIStatus    string           `json:"ai_status"`
	Items       []ReportItem     `json:"items"`
	From        int64            `json:"from"`
	Until       int64            `json:"until"`
	Part        int              `json:"part"`
	More        bool             `json:"more"`
	CoverageGap bool             `json:"coverage_gap"`
}

// ReportCounts counts observations in one part, not distinct files or Git commits.
type ReportCounts struct {
	Added      int `json:"added"`
	Modified   int `json:"modified"`
	Deleted    int `json:"deleted"`
	Deferred   int `json:"deferred"`
	Violations int `json:"violations"`
}

// Counts derives factual totals independently of the optional AI introduction.
func (report *ReportInfo) Counts() ReportCounts {
	if report.Aggregate != nil {
		return report.Aggregate.Counts
	}

	var counts ReportCounts

	for _, item := range report.Items {
		counts.add(item)
	}

	return counts
}

func (counts *ReportCounts) add(item ReportItem) {
	switch item.Detail.Kind {
	case changeAdded:
		counts.Added++
	case changeModified:
		counts.Modified++
	case changeDeleted:
		counts.Deleted++
	}

	if item.Deferred {
		counts.Deferred++
	}

	if item.Violation {
		counts.Violations++
	}
}

type reportPeriod struct {
	Aggregate    *ReportAggregate  `json:"aggregate,omitempty"`
	Destinations map[string]string `json:"destinations"`
	ID           string            `json:"id"`
	After        int64             `json:"after,omitempty"`
	From         int64             `json:"from"`
	Until        int64             `json:"until"`
	Through      int64             `json:"through"`
	Part         int               `json:"part"`
	CoverageGap  bool              `json:"coverage_gap"`
}

type reportState struct {
	Format        string       `json:"format,omitempty"`
	Schedule      string       `json:"schedule"`
	Timezone      string       `json:"timezone"`
	Period        reportPeriod `json:"period"`
	Through       int64        `json:"through,omitempty"`
	Since         int64        `json:"since"`
	Next          int64        `json:"next"`
	Sequence      int64        `json:"sequence"`
	Pending       int64        `json:"pending"`
	LastQueued    int64        `json:"last_queued"`
	LastDelivered int64        `json:"last_delivered"`
	CoverageGap   bool         `json:"coverage_gap"`
}

// ReportDraft is persisted before any outbound AI request. Routing is frozen at period close.
type ReportDraft struct {
	Destinations map[string]string `json:"destinations"`
	Message      Message           `json:"message"`
}

// ReportStatus exposes durable progress without transport credentials.
type ReportStatus struct {
	Next          int64 `json:"next"`
	Since         int64 `json:"since"`
	Pending       int64 `json:"pending"`
	LastQueued    int64 `json:"last_queued"`
	LastDelivered int64 `json:"last_delivered"`
	CoverageGap   bool  `json:"coverage_gap"`
}

// ConfigureReports is called before starting a repository's workers. Disabled reports retain pending evidence.
func (store *Store) ConfigureReports(ctx context.Context, settings config.Reports, restarted bool) error {
	calendar, err := reportclock.Parse(settings.Schedule, settings.Timezone)
	if err != nil {
		return err
	}

	store.mutex.Lock()
	defer store.mutex.Unlock()

	err = ctx.Err()
	if err != nil {
		return fault.Wrap("configure reports", err)
	}

	if settings.Enabled && calendar.Next(store.now()).IsZero() {
		return fault.New("report schedule has no upcoming occurrence")
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	state := &transaction.state.Reports
	if restarted && state.Since != 0 {
		state.CoverageGap = true
	}

	if settings.Enabled {
		if state.Since == 0 {
			state.Since = store.now().UnixNano()
		}

		if state.Next == 0 || state.Schedule != settings.Schedule || state.Timezone != settings.Timezone {
			state.Next = calendar.Next(store.now()).UnixNano()
		}
	} else {
		state.Next = 0
		state.CoverageGap = state.Since != 0
	}

	state.Schedule, state.Timezone = settings.Schedule, settings.Timezone
	state.Format = settings.Format

	err = transaction.commit()
	if err == nil {
		store.reports, store.reportCalendar = settings, calendar
	}

	return err
}

// ReportHead annotates observations with the last known HEAD, not a claimed per-path commit.
func (store *Store) ReportHead(head string) {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	store.reportHead = head
}

// ReportGap records incomplete observation without inventing changes.
func (store *Store) ReportGap(ctx context.Context) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	if !store.reports.Enabled || store.state.Reports.CoverageGap {
		return nil
	}

	err := ctx.Err()
	if err != nil {
		return fault.Wrap("record report gap", err)
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	transaction.state.Reports.CoverageGap = true

	return transaction.commit()
}

// Reports returns the report cursor alongside the ordinary status snapshot.
func (store *Store) Reports() ReportStatus {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	state := store.state.Reports

	return ReportStatus{
		Next: state.Next, Since: state.Since, Pending: state.Pending,
		LastQueued: state.LastQueued, LastDelivered: state.LastDelivered, CoverageGap: state.CoverageGap,
	}
}

func (store *Store) recordReport(transaction *transaction, detail Detail, violation bool) error {
	data, err := get(transaction.batch, key(deferredPrefix, detail.Path))
	if err != nil {
		return err
	}

	transaction.state.Reports.Sequence++
	transaction.state.Reports.Pending++
	item := ReportItem{Detail: detail, Deferred: data != nil, Head: store.reportHead, Violation: violation}
	transaction.put(numberKey(reportJournalPrefix, transaction.state.Reports.Sequence), item)

	return nil
}

// PublishReport atomically transfers a prepared part to each frozen destination and retires its draft.
func (store *Store) PublishReport(ctx context.Context, identifier, summary, aiStatus string) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	err := ctx.Err()
	if err != nil {
		return fault.Wrap("publish report", err)
	}

	draft, err := store.readReportDraft()
	if err != nil || draft == nil {
		return err
	}

	if draft.Message.ChangeID != identifier {
		return fault.New("report draft changed")
	}

	draft.Message.Report.Summary, draft.Message.Report.AIStatus = summary, aiStatus

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	for destination, version := range draft.Destinations {
		transaction.enqueueDelivery(draft.Message, destination, version, store.now())
	}

	transaction.state.Reports.LastQueued = store.now().UnixNano()

	if draft.Message.Report.Aggregate != nil {
		aggregate := draft.Message.Report.Aggregate
		transaction.put([]byte(reportHoldPrefix+draft.Message.Report.ID), reportHold{
			After: aggregate.After, Through: aggregate.Through, Remaining: len(draft.Destinations),
		})
	}

	transaction.remove([]byte(reportDraftKey))

	return transaction.commit()
}

func (store *Store) readReportDraft() (*ReportDraft, error) {
	data, err := get(store.database, []byte(reportDraftKey))
	if err != nil || data == nil {
		return nil, err
	}

	var draft ReportDraft

	err = json.Unmarshal(data, &draft)
	if err != nil {
		return nil, fault.Wrap("decode report draft", err)
	}

	if draft.Message.Report == nil {
		return nil, fault.New("report draft missing period")
	}

	return &draft, nil
}

func reportMessage(store *Store, period reportPeriod, items []ReportItem, more bool) Message {
	return Message{
		Repository: store.Repository, Host: store.Host, Hostname: "", Event: ReportEvent,
		ChangeID: fmt.Sprintf("%s-%d", period.ID, period.Part), Paths: nil, Details: nil, Count: len(items),
		Report: &ReportInfo{
			Aggregate: period.Aggregate,
			ID:        period.ID, From: period.From, Until: period.Until, Part: period.Part,
			More: more, CoverageGap: period.CoverageGap, Items: items, Summary: "", AIStatus: "pending",
		},
	}
}

// carryCoverageGap keeps an unreported gap for the next period: with send_empty false an empty period
// sends nothing, but the next report with evidence must still say that coverage was incomplete.
func carryCoverageGap(transaction *transaction, period reportPeriod, skipped bool) {
	if skipped && period.CoverageGap {
		transaction.state.Reports.CoverageGap = true
	}
}

func (store *Store) finishReportPeriod(transaction *transaction, now time.Time) {
	state := &transaction.state.Reports
	state.Since = state.Period.Until
	state.Through = state.Period.Through
	state.Next = store.reportCalendar.Next(now).UnixNano()
	state.Period = reportPeriod{}
}
