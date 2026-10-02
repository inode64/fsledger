package catalog

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/pathutil"
	"github.com/inode64/fsledger/internal/resource"
)

// ImportBaseline initializes an empty catalog from reviewed JSON observations, never source files.
// Failed imports leave the catalog unapproved; explicitly recreate it before retrying.
func (store *Store) ImportBaseline(ctx context.Context, input io.Reader) error {
	store.operation.Lock()
	defer store.operation.Unlock()

	store.mutex.Lock()
	defer store.mutex.Unlock()

	if store.state.Stats.Entries != 0 || store.state.Stats.BaselineReady ||
		store.state.Stats.PendingNotifications != 0 {
		return fault.New("reference import requires an empty catalog")
	}

	// Import writes reference evidence directly; it never enqueues or sends notifications.
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()

	count := int64(0)

	for {
		records, err := importRecords(decoder)
		if err != nil {
			return err
		}

		if len(records) == 0 {
			break
		}

		err = ctx.Err()
		if err != nil {
			return fault.Wrap("import cancelled", err)
		}

		err = store.importBatch(records)
		if err != nil {
			return err
		}

		count += int64(len(records))
	}

	if count == 0 {
		return fault.New("empty reference input")
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	transaction.state.Stats.BaselineReady = true
	transaction.state.Policy = store.policyID()
	transaction.state.Complete = false // Imported evidence is not a completed scan of this host.

	return transaction.commit()
}

func importRecords(decoder *json.Decoder) ([]integrity.Record, error) {
	records := make([]integrity.Record, 0, transactionBatchSize)
	for range transactionBatchSize {
		var record integrity.Record

		err := decoder.Decode(&record)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return nil, fault.Wrap("decode reference input", err)
		}

		if !pathutil.ValidAbsolute(string(record.Path)) || record.Type == "" {
			return nil, fault.New("invalid reference path or file type")
		}

		records = append(records, record)
	}

	return records, nil
}

func (store *Store) importBatch(records []integrity.Record) error {
	transaction := store.begin()
	defer resource.Close(transaction.batch)

	for _, record := range records {
		old, err := get(transaction.batch, key(currentPrefix, record.Path))
		if err != nil {
			return err
		}

		if old != nil {
			return fault.New("duplicate reference path")
		}

		data, err := packRecord(record)
		if err != nil {
			return err
		}

		transaction.set(key(currentPrefix, record.Path), data)
		transaction.set(key(baselinePrefix(transaction.state.Baseline), record.Path), data)
		transaction.set(key(seenPrefix, record.Path), binary.BigEndian.AppendUint64(nil, 0))
		transaction.state.Stats.Entries++
	}

	return transaction.commit()
}
