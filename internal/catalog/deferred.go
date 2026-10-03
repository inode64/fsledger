package catalog

import (
	"context"
	"encoding/json"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

const deferredPrefix = "f/"

// InitialSnapshotPending survives partial initial commits and daemon restarts.
func (store *Store) InitialSnapshotPending() bool {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	return store.state.InitialSnapshotPending
}

// SetInitialSnapshotPending retains the initial inclusion obligation until a stable scan finishes.
func (store *Store) SetInitialSnapshotPending(ctx context.Context, pending bool) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	err := ctx.Err()
	if err != nil {
		return fault.Wrap("save initial snapshot state", err)
	}

	if store.state.InitialSnapshotPending == pending {
		return nil
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	transaction.state.InitialSnapshotPending = pending

	return transaction.commit()
}

// Deferred identifies the exact staged version whose commit was postponed.
// Keys retain raw Linux path bytes; this is pending state, not history.
type Deferred struct {
	Signature string `json:"signature"`
	Policy    string `json:"policy"`
	Since     int64  `json:"since"`
}

// Deferred loads the durable pending commit state.
func (store *Store) Deferred(ctx context.Context) (map[string]Deferred, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	return store.readDeferred(ctx)
}

func (store *Store) readDeferred(ctx context.Context) (map[string]Deferred, error) {
	result := make(map[string]Deferred)
	err := store.iterate(ctx, deferredPrefix, func(path, data []byte) error {
		var item Deferred

		err := json.Unmarshal(data, &item)
		if err != nil {
			return fault.Wrap("decode deferred path", err)
		}

		result[string(path)] = item

		return nil
	})

	return result, err
}

// SaveDeferred atomically replaces pending versions without rewriting unchanged paths.
func (store *Store) SaveDeferred(ctx context.Context, paths map[string]Deferred) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	cancelErr := ctx.Err()
	if cancelErr != nil {
		return fault.Wrap("save deferred paths", cancelErr)
	}

	previous, err := store.readDeferred(ctx)
	if err != nil {
		return err
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	oldest := int64(0)

	for path, item := range paths {
		if old, exists := previous[path]; !exists || old != item {
			transaction.put(key(deferredPrefix, []byte(path)), item)
		}

		if oldest == 0 || item.Since < oldest {
			oldest = item.Since
		}
	}

	for path := range previous {
		if _, exists := paths[path]; !exists {
			transaction.remove(key(deferredPrefix, []byte(path)))
		}
	}

	transaction.state.Stats.DeferredPaths = int64(len(paths))

	transaction.state.Stats.DeferredSince = oldest
	if transaction.batch.Count() == 0 || ctx.Err() != nil {
		return fault.Wrap("save deferred paths", ctx.Err())
	}

	return transaction.commit()
}
