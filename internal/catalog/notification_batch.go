package catalog

import (
	"cmp"
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

type notificationRoute struct {
	Destination string `json:"destination"`
	Version     string `json:"version"`
}

type notificationBatchKey struct {
	route notificationRoute
	event string
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

func (store *Store) enqueue(transaction *transaction, event, identifier string, details []Detail) {
	if !store.notifies(event) {
		return
	}

	if store.Notifications.BatchWindow <= 0 || len(details) == 0 {
		store.enqueueNow(transaction, event, identifier, "", details)

		return
	}

	routes := store.notificationRoutes()
	for _, detail := range details {
		store.enqueueRoutes(transaction, event, identifier, detail, routes)
	}
}

// Routes already frozen with inventory evidence bypass current subscription
// settings. Recovery must not discard or redirect a previously observed change.
func (store *Store) enqueueRoutes(
	transaction *transaction, event, identifier string, detail Detail, routes []notificationRoute,
) {
	if len(routes) == 0 {
		return
	}

	if store.Notifications.BatchWindow <= 0 {
		for _, route := range routes {
			store.enqueueNowTo(transaction, event, identifier, []Detail{detail}, route)
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
		transaction.notifications = make(map[notificationBatchKey]notificationBatch)
	}

	for _, route := range routes {
		transaction.batchNotification(event, detail, len(encoded), route)
	}
}

// Only combine unpublished records within this transaction. A delivery already
// handed to a sender is immutable, so its acknowledgement cannot erase new evidence.
func (transaction *transaction) batchNotification(event string, detail Detail, size int, route notificationRoute) {
	identifier := detail.ChangeID

	key := notificationBatchKey{event: event, route: route}

	batch, exists := transaction.notifications[key]
	if !exists {
		batch = notificationBatch{identifier: identifier, details: nil, bytes: 0}
	}

	if len(batch.details) >= notificationBatchPaths ||
		(len(batch.details) > 0 && batch.bytes+size > notificationBatchBytes) {
		transaction.store.enqueueNowTo(transaction, event, batch.identifier, batch.details, route)
		batch.details, batch.bytes, batch.identifier = nil, 0, identifier
	}

	if batch.identifier != identifier {
		batch.identifier = ""
	}

	batch.details = append(batch.details, detail)
	batch.bytes += size
	transaction.notifications[key] = batch
}

func (transaction *transaction) flushNotifications() {
	keys := slices.SortedFunc(maps.Keys(transaction.notifications), func(first, second notificationBatchKey) int {
		return cmp.Or(
			cmp.Compare(first.event, second.event),
			cmp.Compare(first.route.Destination, second.route.Destination),
			cmp.Compare(first.route.Version, second.route.Version),
		)
	})
	for _, key := range keys {
		batch := transaction.notifications[key]
		transaction.store.enqueueNowTo(transaction, key.event, batch.identifier, batch.details, key.route)
	}

	transaction.notifications = nil
}
