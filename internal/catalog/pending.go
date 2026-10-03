package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/resource"
)

// Pending records hold no duplicate observations: current and baseline own those values.
type pending struct {
	Token  string   `json:"token"`
	Batch  string   `json:"batch"`
	Policy string   `json:"policy"`
	Kind   string   `json:"kind"`
	Fields []string `json:"fields"`
}

const approvalPathLimit = 4096

var errStop = errors.New(
	"iteration complete",
)

func withoutStop(err error) error {
	if errors.Is(err, errStop) {
		return nil
	}

	return err
}

func readPending(source reader, path []byte) (pending, bool, error) {
	data, err := get(source, key(pendingPrefix, path))
	if err != nil || data == nil {
		return pending{}, false, err
	}

	var result pending

	err = json.Unmarshal(data, &result)

	return result, true, fault.Wrap("decode pending difference", err)
}

func removePending(transaction *transaction, path []byte, old pending, invalidate bool) {
	transaction.remove(key(pendingPrefix, path))
	transaction.remove([]byte(tokenPrefix + old.Token))
	// Invalidating any member invalidates the entire operation approval token.
	if invalidate {
		transaction.clear(approvalPrefix + old.Batch + "/")
	}

	transaction.state.Stats.Violations--
}

func (store *Store) setPending(
	transaction *transaction,
	path []byte,
	old pending,
	exists bool,
	identifier, kind string,
	fields []string,
	changed bool,
) string {
	if len(fields) == 0 {
		if exists {
			removePending(transaction, path, old, true)
		}

		return ""
	}

	if exists && !changed {
		return old.Token
	}

	if exists {
		removePending(transaction, path, old, true)
	}

	item := pending{Token: changeID(), Batch: identifier, Policy: store.policyID(), Kind: kind, Fields: fields}
	transaction.put(key(pendingPrefix, path), item)
	transaction.set([]byte(tokenPrefix+item.Token), path)
	transaction.set(key(approvalPrefix+identifier+"/", path), []byte(item.Token))
	transaction.state.Stats.Violations++

	return item.Token
}

// Changes streams only unresolved differences. Each ID approves exactly one current path version.
func (store *Store) Changes(ctx context.Context, limit int, visit func(string, []byte, string, []byte) error) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	count := 0
	err := store.iterate(ctx, pendingPrefix, func(path, data []byte) error {
		if count >= limit {
			return errStop
		}

		var item pending

		operationErr1 := json.Unmarshal(data, &item)
		if operationErr1 != nil {
			return fault.Wrap("decode pending difference", operationErr1)
		}

		fields, err := encode(item.Fields)
		if err != nil {
			return err
		}

		count++

		return visit(item.Token, path, item.Kind, fields)
	})

	return withoutStop(err)
}

// Accept rejects stale identifiers; a successful notification never implies approval.
func (store *Store) Accept(ctx context.Context, identifier string) error {
	store.operation.Lock()
	defer store.operation.Unlock()

	store.mutex.Lock()
	defer store.mutex.Unlock()

	if !store.state.Stats.BaselineReady {
		return fault.New("initialize baseline before accepting changes")
	}

	if store.state.Policy != store.policyID() {
		return fault.New("baseline policy differs; replace explicitly")
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	path, err := get(store.database, []byte(tokenPrefix+identifier))
	if err != nil {
		return err
	}

	count := 0

	if path != nil {
		err = store.acceptOne(transaction, path, identifier, true)
		count++
	} else {
		err = store.iterate(ctx, approvalPrefix+identifier+"/", func(path, token []byte) error {
			count++
			if count > approvalPathLimit {
				return fault.New(
					"operation exceeds 4096 paths; approve individual IDs or explicitly replace the complete reference",
				)
			}

			return store.acceptOne(transaction, path, string(token), false)
		})
	}

	if err != nil {
		return err
	}

	transaction.clear(approvalPrefix + identifier + "/")

	if count == 0 {
		return fault.New("unknown or stale change ID; list current differences")
	}

	err = ctx.Err()
	if err != nil {
		return fault.Wrap("accept cancelled", err)
	}

	return transaction.commit()
}

func (store *Store) acceptOne(transaction *transaction, path []byte, token string, invalidate bool) error {
	item, exists, err := readPending(transaction.batch, path)
	if err != nil {
		return err
	}

	if !exists || item.Token != token || item.Policy != store.policyID() {
		return fault.New("stale approval; list current differences")
	}

	data, err := get(transaction.batch, key(currentPrefix, path))
	if err != nil {
		return err
	}

	reference := key(baselinePrefix(transaction.state.Baseline), path)
	if data == nil {
		transaction.remove(reference)
	} else {
		err = store.checkApprovalHash(data, path)
		if err != nil {
			return err
		}

		transaction.set(reference, data)
	}

	removePending(transaction, path, item, invalidate)

	return nil
}

func (store *Store) checkApprovalHash(data, path []byte) error {
	if !slices.Contains(store.Policy.Compare, fieldHash) {
		return nil
	}

	record, err := unpack(data, path)
	if err != nil {
		return err
	}

	if record.Type == integrity.TypeRegular &&
		(record.Hash == "" || record.Algorithm != store.Policy.Hash.Algorithm) {
		return fault.New("approval requires a verified content hash; run a full verification first")
	}

	return nil
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
