package catalog

import (
	"context"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/integrity"
)

// Current returns the latest observation, if present.
func (store *Store) Current(ctx context.Context, path []byte) (integrity.Record, bool, error) {
	operationErr4 := ctx.Err()
	if operationErr4 != nil {
		return integrity.Record{}, false, fault.Wrap("read current", operationErr4)
	}

	record, data, err := readRecord(store.database, currentPrefix, path)

	return record, data != nil, err
}
