package catalog

import (
	"context"
	"errors"
	"slices"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

var errStop = errors.New(
	"iteration complete",
)

func withoutStop(err error) error {
	if errors.Is(err, errStop) {
		return nil
	}

	return err
}

// Intent records unfinished coordination with Git; completion removes it.
func (store *Store) Intent(ctx context.Context, identifier string) error {
	return store.writeIntents(ctx, []string{identifier}, []byte{})
}

// CompleteIntent removes one completed Git operation.
func (store *Store) CompleteIntent(ctx context.Context, identifier string) error {
	return store.writeIntents(ctx, []string{identifier}, nil)
}

// CompleteIntents retires recovered operations in bounded durable batches.
func (store *Store) CompleteIntents(ctx context.Context, identifiers []string) error {
	for batch := range slices.Chunk(identifiers, transactionBatchSize) {
		err := store.writeIntents(ctx, batch, nil)
		if err != nil {
			return err
		}
	}

	return fault.Wrap("complete intents", ctx.Err())
}

func (store *Store) writeIntents(ctx context.Context, identifiers []string, message []byte) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	err := ctx.Err()
	if err != nil {
		return fault.Wrap("write intent", err)
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	for _, identifier := range identifiers {
		if message == nil {
			transaction.remove([]byte(intentPrefix + identifier))
		} else {
			transaction.set([]byte(intentPrefix+identifier), message)
		}
	}

	return transaction.commit()
}

// PendingIntents lists only unfinished Git operations.
func (store *Store) PendingIntents(ctx context.Context) ([]string, error) {
	var identifiers []string

	err := store.iterate(
		ctx,
		intentPrefix,
		func(identifier, _ []byte) error {
			identifiers = append(identifiers, string(identifier))

			return nil
		},
	)

	return identifiers, err
}
