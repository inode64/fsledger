package catalog

import (
	"encoding/json"
	"maps"
	"slices"

	"github.com/inode64/fsledger/internal/fault"
)

const (
	notificationBatchPaths = 64
	notificationBatchBytes = 16 << 10
)

type notificationBatch struct {
	identifier string
	details    []Detail
	bytes      int
}

func (store *Store) notifies(event string) bool {
	return len(store.Notifications.Use) > 0 && slices.Contains(store.Notifications.Events, event)
}

func (store *Store) enqueue(transaction *transaction, event, identifier string, details []Detail) {
	if !store.notifies(event) {
		return
	}

	if store.Notifications.BatchWindow <= 0 || len(details) == 0 {
		store.enqueueNow(transaction, event, identifier, "", details)

		return
	}

	if transaction.notifications == nil {
		transaction.notifications = make(map[string]notificationBatch)
	}

	for _, detail := range details {
		transaction.batchNotification(event, identifier, detail)
	}
}

// Only combine unpublished records within this transaction. A delivery already
// handed to a sender is immutable, so its acknowledgement cannot erase new evidence.
func (transaction *transaction) batchNotification(event, identifier string, detail Detail) {
	detail.ChangeID = identifier

	encoded, err := json.Marshal(detail)
	if err != nil {
		transaction.err = fault.Wrap("encode notification evidence", err)

		return
	}

	batch, exists := transaction.notifications[event]
	if !exists {
		batch = notificationBatch{identifier: identifier, details: nil, bytes: 0}
	}

	if len(batch.details) >= notificationBatchPaths ||
		(len(batch.details) > 0 && batch.bytes+len(encoded) > notificationBatchBytes) {
		transaction.store.enqueueNow(transaction, event, batch.identifier, "", batch.details)
		batch.details, batch.bytes, batch.identifier = nil, 0, identifier
	}

	if batch.identifier != identifier {
		batch.identifier = ""
	}

	batch.details = append(batch.details, detail)
	batch.bytes += len(encoded)
	transaction.notifications[event] = batch
}

func (transaction *transaction) flushNotifications() {
	for _, event := range slices.Sorted(maps.Keys(transaction.notifications)) {
		batch := transaction.notifications[event]
		transaction.store.enqueueNow(transaction, event, batch.identifier, "", batch.details)
	}

	transaction.notifications = nil
}
