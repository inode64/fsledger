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

// Delivery tracks one destination independently; acknowledgements remove it permanently.
type Delivery struct {
	Destination string  `json:"destination"`
	Version     string  `json:"version"`
	LastError   string  `json:"last_error,omitempty"`
	Message     Message `json:"message"`
	ID          int64   `json:"id"`
	Attempts    int     `json:"attempts"`
	NextAttempt int64   `json:"next_attempt"`
}

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
	paths := make([]string, len(details))
	for index := range details {
		paths[index] = string(details[index].Path)
	}

	// The kernel hostname is evidence of where the change was queued; it must never block delivery.
	hostname, err := os.Hostname()
	if err != nil {
		hostname = ""
	}

	message := Message{
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
	for _, destination := range store.Notifications.Use {
		transaction.enqueueDelivery(message, destination, store.versions[destination],
			store.notificationDue(destination, event, store.now()))
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
		Destination: destination, Version: version, Message: message,
		ID: transaction.state.NextDelivery, NextAttempt: due.UnixNano(), LastError: "", Attempts: 0,
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
	return store.updateDeliveries(ctx, identifiers, 0, "", false)
}

// RetryMany records a shared retry schedule while preserving per-delivery attempts.
func (store *Store) RetryMany(ctx context.Context, identifiers []int64, delay time.Duration, reason string) error {
	return store.updateDeliveries(ctx, identifiers, max(delay, time.Millisecond), reason, true)
}

// DeferMany schedules a shared local transport slot without failed attempts.
func (store *Store) DeferMany(ctx context.Context, identifiers []int64, delay time.Duration) error {
	return store.updateDeliveries(ctx, identifiers, max(delay, time.Millisecond), "", false)
}

//nolint:funlen // One Pebble transaction must validate and update the complete delivery set.
func (store *Store) updateDeliveries(
	ctx context.Context,
	identifiers []int64,
	delay time.Duration,
	reason string,
	attempted bool,
) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	operationErr1 := ctx.Err()
	if operationErr1 != nil {
		return fault.Wrap("update delivery", operationErr1)
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	now := store.now()
	seen := make(map[int64]bool, len(identifiers))

	for _, identifier := range identifiers {
		if seen[identifier] {
			return fault.New("duplicate delivery identifier")
		}

		seen[identifier] = true

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

		transaction.remove(deliveryDue(delivery))

		if delay == 0 {
			transaction.acknowledge(delivery, now)

			continue
		}

		if attempted {
			delivery.Attempts++
			delivery.LastError = reason
		}

		delivery.NextAttempt = now.Add(delay).UnixNano()
		transaction.put(numberKey(outboxPrefix, identifier), delivery)
		transaction.set(deliveryDue(delivery), nil)
	}

	return transaction.commit()
}

// DiscardPending drops every queued delivery, due or delayed, in one durable transaction. Summary
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
}
