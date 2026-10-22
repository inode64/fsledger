package catalog

import (
	"cmp"
	"encoding/json"
	"maps"
	"slices"

	"github.com/inode64/fsledger/internal/config"
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

type notificationRoute struct {
	Destination string `json:"destination"`
	Version     string `json:"version"`
}

func (store *Store) notificationRoutes() []notificationRoute {
	routes := make([]notificationRoute, 0, len(store.Notifications.Use))
	for _, destination := range store.Notifications.Use {
		routes = append(routes, notificationRoute{Destination: destination, Version: store.versions[destination]})
	}

	return routes
}

func (store *Store) notifies(event string) bool {
	return len(store.Notifications.Use) > 0 && slices.Contains(store.Notifications.Events, event)
}

// Routes already frozen with inventory evidence bypass current subscription
// settings. Recovery must not discard or redirect a previously observed change.
func (store *Store) enqueueRoutes(
	transaction *transaction,
	identifier string,
	detail Detail,
	routes []notificationRoute,
) {
	if len(routes) == 0 {
		return
	}

	if store.Notifications.BatchWindow <= 0 {
		for _, route := range routes {
			store.enqueueChange(transaction, identifier, []Detail{detail}, route)
		}

		return
	}

	detail.ChangeID = identifier

	encoded, err := json.Marshal(detail)
	if err != nil {
		transaction.err = fault.Wrap("encode notification evidence", err)

		return
	}

	if transaction.notifications == nil {
		transaction.notifications = make(map[notificationRoute]notificationBatch)
	}

	for _, route := range routes {
		transaction.batchNotification(detail, len(encoded), route)
	}
}

// Only combine unpublished records within this transaction. A delivery already
// handed to a sender is immutable, so its acknowledgement cannot erase new evidence.
func (transaction *transaction) batchNotification(detail Detail, size int, route notificationRoute) {
	identifier := detail.ChangeID

	batch, exists := transaction.notifications[route]
	if !exists {
		batch = notificationBatch{identifier: identifier, details: nil, bytes: 0}
	}

	if len(batch.details) >= notificationBatchPaths ||
		(len(batch.details) > 0 && batch.bytes+size > notificationBatchBytes) {
		transaction.store.enqueueChange(transaction, batch.identifier, batch.details, route)
		batch.details, batch.bytes, batch.identifier = nil, 0, identifier
	}

	if batch.identifier != identifier {
		batch.identifier = ""
	}

	batch.details = append(batch.details, detail)
	batch.bytes += size
	transaction.notifications[route] = batch
}

func (transaction *transaction) flushNotifications() {
	routes := slices.SortedFunc(maps.Keys(transaction.notifications), func(first, second notificationRoute) int {
		return cmp.Or(cmp.Compare(first.Destination, second.Destination), cmp.Compare(first.Version, second.Version))
	})
	for _, route := range routes {
		batch := transaction.notifications[route]
		transaction.store.enqueueChange(transaction, batch.identifier, batch.details, route)
	}

	transaction.notifications = nil
}

// enqueueChange persists one change message for route.
func (store *Store) enqueueChange(
	transaction *transaction,
	identifier string,
	details []Detail,
	route notificationRoute,
) {
	store.enqueueTo(transaction, store.notificationMessage(config.NotificationChange, identifier, "", details), route)
}
