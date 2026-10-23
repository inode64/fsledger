package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/cockroachdb/pebble/v2"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/resource"
)

const stagedChangeKeySeparator = "\x00"

// stagedChange retains the first and last observed versions of a path in an
// operation. It is committed with the inventory update, so a restart can finish
// its notification without losing evidence or relying on an in-memory buffer.
type stagedChange struct {
	Before   *integrity.Record   `json:"before,omitempty"`
	After    *integrity.Record   `json:"after,omitempty"`
	ID       string              `json:"id"`
	Actor    string              `json:"actor"`
	Routes   []notificationRoute `json:"routes"`
	Path     []byte              `json:"path"`
	Observed int64               `json:"observed"`
	Notify   bool                `json:"notify"`
}

func decodeStagedChange(data []byte) (stagedChange, error) {
	var staged stagedChange

	err := json.Unmarshal(data, &staged)
	if err != nil {
		return stagedChange{}, fault.Wrap("decode staged change", err)
	}

	return staged, nil
}

func stagedChangeKey(identifier string, path []byte) []byte {
	return key(notificationStagePrefix+identifier+stagedChangeKeySeparator, path)
}

type changeNotification struct {
	deliver bool
	stage   bool
}

func (store *Store) enqueueObservedChange(
	transaction *transaction,
	record integrity.Record,
	compared *comparison,
	detail Detail,
	identifier string,
	notification changeNotification,
) error {
	if !notification.stage {
		if notification.deliver {
			store.enqueueRoutes(transaction, identifier, detail, store.notificationRoutes())
		}

		return nil
	}

	lookup := stagedChangeKey(identifier, record.Path)

	data, err := get(transaction.batch, lookup)
	if err != nil {
		return err
	}

	var staged stagedChange
	if data != nil {
		staged, err = decodeStagedChange(data)
		if err != nil {
			return err
		}
	} else {
		staged.Path = slices.Clone(record.Path)
		staged.ID = identifier
		staged.Routes = store.notificationRoutes()

		if compared.oldData != nil {
			previous := compared.previous
			staged.Before = &previous
		}
	}

	staged.observe(record, compared.currentData != nil, detail.Actor, detail.Observed)
	staged.Notify = notification.deliver
	transaction.put(lookup, staged)

	return transaction.err
}

// Metadata or hash availability can change without a difference in compared
// fields. A pending notification must still describe the last observed version.
func (store *Store) refreshStagedObservation(
	transaction *transaction,
	record integrity.Record,
	compared *comparison,
	identifier string,
) error {
	if store.notificationOperation != identifier || bytes.Equal(compared.oldData, compared.currentData) {
		return nil
	}

	lookup := stagedChangeKey(identifier, record.Path)

	data, err := get(transaction.batch, lookup)
	if err != nil || data == nil {
		return err
	}

	staged, err := decodeStagedChange(data)
	if err != nil {
		return err
	}

	staged.observe(record, compared.currentData != nil, staged.Actor, observedAt(record))
	transaction.put(lookup, staged)

	return transaction.err
}

func (staged *stagedChange) observe(record integrity.Record, exists bool, actor string, observed int64) {
	staged.After = nil
	if exists {
		staged.After = &record
	}

	staged.Actor = actor
	staged.Observed = observed
}

func (store *Store) startNotificationOperation(ctx context.Context, identifier string) error {
	err := store.finishStagedChanges(ctx)
	if err != nil {
		return err
	}

	store.notificationOperation = identifier

	return nil
}

// The caller holds store.operation throughout inventory updates and publication.
func (store *Store) withNotificationOperation(
	ctx context.Context,
	identifier string,
	observe func(string) (Result, error),
) (Result, error) {
	result := observationResult(identifier)

	err := store.startNotificationOperation(ctx, result.ID)
	if err != nil {
		return result, err
	}
	defer func() { store.notificationOperation = "" }()

	result, err = observe(result.ID)

	return result, errors.Join(err, store.finishStagedChanges(ctx))
}

type stagedChangeEntry struct {
	lookup []byte
	change stagedChange
}

// finishStagedChanges publishes bounded batches. Removing a stage record and
// queuing its delivery share a transaction, including during crash recovery.
// Each batch resumes after the previous one instead of skipping its tombstones again.
func (store *Store) finishStagedChanges(ctx context.Context) error {
	lower := []byte(notificationStagePrefix)

	for {
		entries, err := store.readStagedChanges(ctx, lower)
		if err != nil {
			return err
		}

		if len(entries) == 0 {
			return nil
		}

		err = store.publishStagedChanges(entries)
		if err != nil {
			return err
		}

		lower = append(slices.Clone(entries[len(entries)-1].lookup), 0)
	}
}

func (store *Store) readStagedChanges(ctx context.Context, lower []byte) ([]stagedChangeEntry, error) {
	options := &pebble.IterOptions{}
	options.LowerBound, options.UpperBound = lower, upper(notificationStagePrefix)

	entries := make([]stagedChangeEntry, 0, transactionBatchSize)
	err := store.iterateRange(ctx, notificationStagePrefix, options, func(path, data []byte) error {
		if len(entries) == transactionBatchSize {
			return errStop
		}

		staged, err := decodeStagedChange(data)
		if err != nil {
			return err
		}

		entries = append(entries, stagedChangeEntry{lookup: key(notificationStagePrefix, path), change: staged})

		return nil
	})

	return entries, withoutStop(err)
}

func (store *Store) publishStagedChanges(entries []stagedChangeEntry) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	for _, entry := range entries {
		detail, err := entry.change.netChange(store.policy)
		if err != nil {
			return fault.Wrap("finalize staged change", err)
		}

		if entry.change.Notify && detail.Kind != "" {
			store.enqueueRoutes(transaction, entry.change.ID, detail, entry.change.Routes)
		}

		transaction.remove(entry.lookup)
	}

	return transaction.commit()
}

func (staged *stagedChange) netChange(policy config.Integrity) (Detail, error) {
	previous := integrity.Record{Path: staged.Path}

	current := integrity.Record{Path: staged.Path}
	if staged.Before != nil {
		previous = *staged.Before
	}

	if staged.After != nil {
		current = *staged.After
	}

	fields := verifiedFields(
		previous,
		differences(previous, current, staged.Before != nil, staged.After != nil, policy),
	)

	if len(fields) == 0 && !hasUnverifiedContent(previous, current) {
		return Detail{}, nil
	}

	return newDetail(previous, current, fields, staged.Actor, staged.Observed)
}

// Keep evidence when the observed versions' content cannot be compared. Identical observations do not
// introduce new uncertainty.
func hasUnverifiedContent(previous, current integrity.Record) bool {
	return previous.Type == integrity.TypeRegular && current.Type == integrity.TypeRegular &&
		(previous.Hash == "" || current.Hash == "") && !sameObservation(previous, current)
}
