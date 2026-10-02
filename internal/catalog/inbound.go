package catalog

import (
	"context"
	"encoding/json"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/inbound"
	"github.com/inode64/fsledger/internal/resource"
)

const inboundKey = "meta/inbound"

// Incoming loads the durable incoming operation before ordinary reconciliation.
func (store *Store) Incoming(ctx context.Context) (*inbound.Plan, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	err := ctx.Err()
	if err != nil {
		return nil, fault.Wrap("read incoming operation", err)
	}

	data, err := get(store.database, []byte(inboundKey))
	if err != nil || data == nil {
		return nil, err
	}

	var plan inbound.Plan

	err = json.Unmarshal(data, &plan)

	return &plan, fault.Wrap("decode incoming operation", err)
}

// SaveIncoming commits progress synchronously; a nil plan clears completed work.
func (store *Store) SaveIncoming(ctx context.Context, plan *inbound.Plan) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	err := ctx.Err()
	if err != nil {
		return fault.Wrap("save incoming operation", err)
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	if plan == nil {
		transaction.remove([]byte(inboundKey))
	} else {
		transaction.put([]byte(inboundKey), plan)
	}

	return transaction.commit()
}
