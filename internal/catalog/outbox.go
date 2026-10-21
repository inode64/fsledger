package catalog

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

// FieldChange carries printable values; binary metadata uses JSON/base64 encoding.
type FieldChange struct {
	Field  string `json:"field"`
	Before string `json:"before"`
	After  string `json:"after"`
}

// Detail is self-contained evidence for one path, independent of local approval state.
type Detail struct {
	ChangeID string        `json:"change_id,omitempty"`
	Kind     string        `json:"kind"`
	Actor    string        `json:"actor"`
	Path     []byte        `json:"path"`
	Fields   []FieldChange `json:"fields"`
	Baseline []FieldChange `json:"baseline,omitempty"`
	Observed int64         `json:"observed"`
}

// Message never contains source file contents or transport credentials.
type Message struct {
	Report     *ReportInfo `json:"report,omitempty"`
	Repository string      `json:"repository"`
	Host       string      `json:"host"`
	Hostname   string      `json:"hostname,omitempty"`
	Event      string      `json:"event"`
	ChangeID   string      `json:"change_id"`
	Reason     string      `json:"reason,omitempty"`
	Paths      []string    `json:"paths"`
	Details    []Detail    `json:"details,omitempty"`
	Count      int         `json:"count"`
}

// Added counts added paths. Counts derive from Details, so already queued messages need no migration.
func (message Message) Added() int { return message.countKind(changeAdded) }

// Modified counts modified paths.
func (message Message) Modified() int { return message.countKind(changeModified) }

// Deleted counts deleted paths.
func (message Message) Deleted() int { return message.countKind(changeDeleted) }

func (message Message) countKind(kind string) int {
	count := 0

	for _, detail := range message.Details {
		if detail.Kind == kind {
			count++
		}
	}

	return count
}

// Progress is the prefix of one rendered transport message that its destination already accepted.
// Fingerprint identifies that exact rendering, so a retry resumes only while it renders the same text.
type Progress struct {
	Fingerprint string `json:"fingerprint"`
	Sent        int    `json:"sent"`
}

// Delivery tracks one destination independently; acknowledgements remove it permanently.
// Deliveries queued before Progress existed decode without it and start from the first part.
// Exhausted marks a delivery that stopped retrying; it stays queued, outside the due index.
type Delivery struct {
	Destination string   `json:"destination"`
	Version     string   `json:"version"`
	LastError   string   `json:"last_error,omitempty"`
	Progress    Progress `json:"progress,omitzero"`
	Message     Message  `json:"message"`
	ID          int64    `json:"id"`
	Attempts    int      `json:"attempts"`
	NextAttempt int64    `json:"next_attempt"`
	Exhausted   bool     `json:"exhausted,omitzero"`
}

// MaxDeliveryAttempts failed attempts stop the automatic retries of one delivery. The sender's backoff
// doubles from two seconds to about 68 minutes, so they span about a day. The delivery keeps its evidence
// until a daemon restart retries it or outbox clear discards it.
const MaxDeliveryAttempts = 32

type notificationWindow struct {
	due      time.Time
	last     time.Time
	duration time.Duration
}

const (
	numberBytes        = 8
	windowGrowthFactor = 2
)

//nolint:gosec // Signed timestamps and positive sequence IDs preserve their bits in internal big-endian keys.
func numberKey(prefix string, value int64) []byte {
	return binary.BigEndian.AppendUint64([]byte(prefix), uint64(value))
}

//nolint:gosec // Delivery IDs are allocated as positive signed counters.
func deliveryDue(delivery Delivery) []byte {
	return binary.BigEndian.AppendUint64(numberKey(duePrefix, delivery.NextAttempt), uint64(delivery.ID))
}

// enqueueNow persists a delivery; enqueue has already dropped events that are not enabled.
func (store *Store) enqueueNow(transaction *transaction, event, identifier, reason string, details []Detail) {
	message := store.notificationMessage(event, identifier, reason, details)
	for _, route := range store.notificationRoutes() {
		transaction.enqueueDelivery(message, route.Destination, route.Version,
			store.notificationDue(route.Destination, event, store.now()))
	}
}

func (store *Store) enqueueNowTo(
	transaction *transaction, event, identifier string, details []Detail, route notificationRoute,
) {
	message := store.notificationMessage(event, identifier, "", details)
	transaction.enqueueDelivery(message, route.Destination, route.Version,
		store.notificationDue(route.Destination, event, store.now()))
}

func (store *Store) notificationMessage(event, identifier, reason string, details []Detail) Message {
	paths := make([]string, len(details))
	for index := range details {
		paths[index] = string(details[index].Path)
	}

	// The kernel hostname is evidence of where the change was queued; it must never block delivery.
	hostname, err := os.Hostname()
	if err != nil {
		hostname = ""
	}

	return Message{
		Report:     nil,
		Repository: store.Repository,
		Host:       store.Host,
		Hostname:   hostname,
		Event:      event,
		ChangeID:   identifier,
		Reason:     reason,
		Paths:      paths,
		Count:      len(details),
		Details:    details,
	}
}

// enqueueDelivery owns the durable routing record, due index and backlog counters.
// Callers choose the delivery time and freeze the destination version beforehand.
func (transaction *transaction) enqueueDelivery(message Message, destination, version string, due time.Time) {
	store := transaction.store
	// A soft watermark reports backlog without silently destroying the sole event evidence.
	if transaction.state.Stats.PendingNotifications >= int64(store.Notifications.MaxPending) {
		transaction.state.Stats.NotificationOverflow++
		if transaction.state.Stats.PendingNotifications == int64(store.Notifications.MaxPending) {
			slog.Warn("notification backlog exceeds configured watermark", "repository", store.Repository)
		}
	}

	transaction.state.NextDelivery++
	delivery := Delivery{
		Destination: destination, Version: version, Message: message, Progress: Progress{},
		ID: transaction.state.NextDelivery, NextAttempt: due.UnixNano(), LastError: "", Attempts: 0,
		Exhausted: false,
	}
	transaction.put(numberKey(outboxPrefix, delivery.ID), delivery)
	transaction.set(deliveryDue(delivery), nil)
	transaction.state.Stats.PendingNotifications++
}

func (store *Store) notificationDue(destination, event string, now time.Time) time.Time {
	base := store.Notifications.BatchWindow
	if base <= 0 {
		return now
	}

	if !digestible(event) {
		return now.Add(base)
	}

	// Validation keeps max_batch_window at or above batch_window.
	maximum := store.Notifications.MaxBatchWindow

	window, exists := store.windows[destination]
	switch {
	case !exists || now.Sub(window.last) > maximum:
		window = notificationWindow{due: now.Add(base), last: now, duration: base}
	case now.Before(window.due):
		window.last = now
	default:
		window.duration = min(windowGrowthFactor*window.duration, maximum)
		window.due = now.Add(window.duration)
		window.last = now
	}

	store.windows[destination] = window

	return window.due
}

// Announce queues an operational event in a durable transaction. The reason names the warnings behind an
// error, or what a recovery recovered from; without it the message carries no evidence at all.
func (store *Store) Announce(ctx context.Context, event, reason string) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	err := ctx.Err()
	if err != nil {
		return fault.Wrap("announce", err)
	}

	if !store.notifies(event) {
		return nil
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	store.enqueueNow(transaction, event, changeID(), reason, nil)

	return transaction.commit()
}

// Due returns at most 1024 due messages without scanning a delayed backlog.
func (store *Store) Due(ctx context.Context) ([]Delivery, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	const limit = 1024

	var deliveries []Delivery

	err := store.iterate(ctx, duePrefix, func(suffix, _ []byte) error {
		if len(suffix) != 2*numberBytes {
			return fault.New("invalid delivery schedule key")
		}

		if binary.BigEndian.Uint64(suffix[:8]) > uint64(store.now().UnixNano()) || len(deliveries) == limit {
			return errStop
		}

		data, err := get(store.database, append([]byte(outboxPrefix), suffix[numberBytes:]...))
		if err != nil {
			return err
		}

		var delivery Delivery

		err = json.Unmarshal(data, &delivery)
		if err != nil {
			return fault.Wrap("decode delivery", err)
		}

		deliveries = append(deliveries, delivery)

		return nil
	})

	return deliveries, withoutStop(err)
}

// DeliveredMany acknowledges every delivery in one durable transaction.
func (store *Store) DeliveredMany(ctx context.Context, identifiers []int64) error {
	_, err := store.updateDeliveries(ctx, identifiers, 0, "", false)

	return err
}

// RetryMany records a shared retry schedule while preserving per-delivery attempts. It returns how many
// deliveries reached MaxDeliveryAttempts with this failure and stopped retrying.
func (store *Store) RetryMany(
	ctx context.Context, identifiers []int64, delay time.Duration, reason string,
) (int, error) {
	return store.updateDeliveries(ctx, identifiers, max(delay, time.Millisecond), reason, true)
}

// DeferMany schedules a shared local transport slot without failed attempts.
func (store *Store) DeferMany(ctx context.Context, identifiers []int64, delay time.Duration) error {
	_, err := store.updateDeliveries(ctx, identifiers, max(delay, time.Millisecond), "", false)

	return err
}

// RetryExhausted gives every delivery that stopped retrying a new retry cycle, due now, while its frozen
// destination is still configured unchanged. The daemon calls it at start, so a restart after repairing a
// relay, credentials or a template sends them. Deliveries frozen for a removed or changed destination stay
// exhausted: they are never redirected. The scan also recounts them, since an earlier version rewrites the
// catalog state without that counter.
func (store *Store) RetryExhausted(ctx context.Context) (int, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	var deliveries []Delivery

	kept := int64(0)

	err := store.iterate(ctx, outboxPrefix, func(_, data []byte) error {
		var delivery Delivery

		err := json.Unmarshal(data, &delivery)
		if err != nil {
			return fault.Wrap("decode delivery", err)
		}

		if !delivery.Exhausted {
			return nil
		}

		version, configured := store.versions[delivery.Destination]
		if configured && (delivery.Version == "" || delivery.Version == version) {
			deliveries = append(deliveries, delivery)
		} else {
			kept++
		}

		return nil
	})
	if err != nil {
		return 0, err
	}

	if len(deliveries) == 0 && kept == store.state.Stats.ExhaustedNotifications {
		return 0, nil
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	now := store.now()

	for _, delivery := range deliveries {
		delivery.Exhausted, delivery.Attempts, delivery.NextAttempt = false, 0, now.UnixNano()
		transaction.put(numberKey(outboxPrefix, delivery.ID), delivery)
		transaction.set(deliveryDue(delivery), nil)
	}

	transaction.state.Stats.ExhaustedNotifications = kept

	err = transaction.commit()
	if err != nil {
		return 0, err
	}

	return len(deliveries), nil
}

// ProgressMany records, in one durable transaction, the prefix of a multipart message accepted for every
// delivery it represents. Schedule, attempts and last error are unchanged, so a retry after a failure,
// restart or crash resumes after that prefix instead of posting it again.
func (store *Store) ProgressMany(ctx context.Context, identifiers []int64, progress Progress) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	err := ctx.Err()
	if err != nil {
		return fault.Wrap("record delivery progress", err)
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	for _, identifier := range identifiers {
		data, err := get(store.database, numberKey(outboxPrefix, identifier))
		if err != nil {
			return err
		}

		if data == nil {
			continue
		}

		var delivery Delivery

		err = json.Unmarshal(data, &delivery)
		if err != nil {
			return fault.Wrap("decode delivery", err)
		}

		delivery.Progress = progress
		transaction.put(numberKey(outboxPrefix, identifier), delivery)
	}

	return transaction.commit()
}

//nolint:funlen // One Pebble transaction must validate and update the complete delivery set.
func (store *Store) updateDeliveries(
	ctx context.Context,
	identifiers []int64,
	delay time.Duration,
	reason string,
	attempted bool,
) (int, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	operationErr1 := ctx.Err()
	if operationErr1 != nil {
		return 0, fault.Wrap("update delivery", operationErr1)
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	now := store.now()
	seen := make(map[int64]bool, len(identifiers))
	exhausted := 0

	for _, identifier := range identifiers {
		if seen[identifier] {
			return 0, fault.New("duplicate delivery identifier")
		}

		seen[identifier] = true

		data, err := get(store.database, numberKey(outboxPrefix, identifier))
		if err != nil {
			return 0, err
		}

		if data == nil {
			continue
		}

		var delivery Delivery

		err = json.Unmarshal(data, &delivery)
		if err != nil {
			return 0, fault.Wrap("decode delivery", err)
		}

		transaction.remove(deliveryDue(delivery))

		if delay == 0 {
			transaction.acknowledge(delivery, now)

			continue
		}

		if transaction.reschedule(delivery, now.Add(delay), reason, attempted) {
			exhausted++
		}
	}

	err := transaction.commit()
	if err != nil {
		return 0, err
	}

	return exhausted, nil
}

// reschedule records a failed attempt or a local deferral. The failure that reaches MaxDeliveryAttempts
// stops the retries and returns true: the delivery stays queued and counted, outside the due index, so it
// delays neither later deliveries nor their transport slots.
func (transaction *transaction) reschedule(delivery Delivery, due time.Time, reason string, attempted bool) bool {
	if delivery.Exhausted {
		// Only a restart, an acknowledgement or outbox clear changes a delivery that stopped retrying.
		return false
	}

	if attempted {
		delivery.Attempts++
		delivery.LastError = reason
	}

	if attempted && delivery.Attempts >= MaxDeliveryAttempts {
		delivery.Exhausted = true
		transaction.put(numberKey(outboxPrefix, delivery.ID), delivery)
		transaction.state.Stats.ExhaustedNotifications++

		return true
	}

	delivery.NextAttempt = due.UnixNano()
	transaction.put(numberKey(outboxPrefix, delivery.ID), delivery)
	transaction.set(deliveryDue(delivery), nil)

	return false
}

// DiscardPending drops every queued delivery, due, delayed or exhausted, in one durable transaction. Summary
// evidence held for a discarded report is released as if it had been sent, but no report is recorded
// as delivered. Recorded changes, baselines and violations are untouched.
func (store *Store) DiscardPending(ctx context.Context) (int, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	var deliveries []Delivery

	err := store.iterate(ctx, outboxPrefix, func(_, data []byte) error {
		var delivery Delivery

		err := json.Unmarshal(data, &delivery)
		if err != nil {
			return fault.Wrap("decode delivery", err)
		}

		deliveries = append(deliveries, delivery)

		return nil
	})
	if err != nil {
		return 0, err
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	for _, delivery := range deliveries {
		transaction.remove(deliveryDue(delivery))

		if delivery.Message.Event == ReportEvent {
			transaction.acknowledgeReportSummary(delivery.Message)
		}

		transaction.remove(numberKey(outboxPrefix, delivery.ID))
		transaction.state.Stats.PendingNotifications--

		if delivery.Exhausted {
			transaction.state.Stats.ExhaustedNotifications--
		}
	}

	err = transaction.commit()
	if err != nil {
		return 0, err
	}

	return len(deliveries), nil
}

func (transaction *transaction) acknowledge(delivery Delivery, now time.Time) {
	if delivery.Message.Event == ReportEvent {
		transaction.state.Reports.LastDelivered = now.UnixNano()
		transaction.acknowledgeReportSummary(delivery.Message)
	}

	transaction.remove(numberKey(outboxPrefix, delivery.ID))
	transaction.state.Stats.PendingNotifications--

	if delivery.Exhausted {
		transaction.state.Stats.ExhaustedNotifications--
	}
}
