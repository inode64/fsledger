package catalog

import (
	"context"
	"time"

	"github.com/cockroachdb/pebble/v2"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

// ReadReport opens an existing catalog read-only, without initializing inventory or consuming evidence.
// Pebble's exclusive lock requires the owning daemon to be stopped.
func ReadReport(ctx context.Context, path, repository, host string) (Message, error) {
	err := inspectDirectory(path)
	if err != nil {
		return Message{}, err
	}

	options := &pebble.Options{}
	options.ReadOnly = true
	options.Logger = engineLogger{repository: repository}

	database, err := pebble.Open(path, options)
	if err != nil {
		return Message{}, fault.Wrap("open report catalog (stop its daemon first)", err)
	}
	defer resource.Close(database)

	store := new(Store)
	store.database, store.now = database, time.Now
	store.Repository, store.Host = repository, host

	format, err := get(database, []byte(formatKey))
	if err != nil {
		return Message{}, err
	}

	if format == nil {
		return Message{}, fault.New("report catalog is not initialized")
	}

	err = store.loadState(format)
	if err != nil {
		return Message{}, err
	}

	return store.PreviewReport(ctx)
}
