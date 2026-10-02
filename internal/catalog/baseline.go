package catalog

import (
	"context"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

// InitBaseline approves a complete inventory without keeping old reference versions.
func (store *Store) InitBaseline(ctx context.Context) error { return store.freezeBaseline(ctx, false) }

// ReplaceBaseline explicitly replaces the reference after a complete scan.
func (store *Store) ReplaceBaseline(ctx context.Context) error {
	return store.freezeBaseline(ctx, true)
}

func (store *Store) freezeBaseline(ctx context.Context, replace bool) error {
	store.operation.Lock()
	defer store.operation.Unlock()

	store.mutex.Lock()
	defer store.mutex.Unlock()

	if !store.state.Complete {
		return fault.New("baseline requires a complete successful inventory scan")
	}

	if store.state.Stats.BaselineReady && !replace {
		return fault.New("baseline already exists; accept specific change IDs instead")
	}

	slot := 1 - store.state.Baseline

	err := store.clearSlot(slot)
	if err != nil {
		return err
	}

	err = store.copyBaseline(ctx, slot)
	if err != nil {
		return err
	}

	err = ctx.Err()
	if err != nil {
		return fault.Wrap("publish baseline", err)
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	transaction.clear(baselinePrefix(store.state.Baseline))
	transaction.clear(pendingPrefix)
	transaction.clear(tokenPrefix)
	transaction.clear(approvalPrefix)
	transaction.state.Baseline = slot
	transaction.state.Policy = store.policyID()
	transaction.state.Stats.BaselineReady = true
	transaction.state.Stats.Violations = 0

	return transaction.commit()
}

func (store *Store) clearSlot(slot int) error {
	transaction := store.begin()
	defer resource.Close(transaction.batch)

	transaction.clear(baselinePrefix(slot))

	return transaction.commit()
}

func (store *Store) copyBaseline(ctx context.Context, slot int) error {
	// Bounded durable batches fill an invisible slot. Only the final pointer switch publishes it.
	batch := store.database.NewBatch()
	defer resource.Close(batch)

	count := 0

	err := store.iterate(ctx, currentPrefix, func(path, data []byte) error {
		err := batch.Set(key(baselinePrefix(slot), path), data, nil)
		if err != nil {
			return fault.Wrap("copy reference", err)
		}

		count++
		if count < transactionBatchSize {
			return nil
		}

		err = commitBatch(batch)
		if err != nil {
			return err
		}

		batch.Reset()

		count = 0

		return nil
	})
	if err != nil {
		return err
	}

	return commitBatch(batch)
}
