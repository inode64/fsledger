package catalog

import (
	"context"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

// OpenRepository opens a writable catalog with its effective policy, destinations and reports.
// The caller holds the repository lock and owns the returned store.
func OpenRepository(ctx context.Context, cfg *config.Config, name string) (*Store, error) {
	repo, exists := cfg.Repositories[name]
	if !exists {
		return nil, fault.New("catalog repository missing: " + name)
	}

	store, err := Open(ctx, cfg.CatalogPath(name), name, cfg.Server.Name, repo.Integrity, repo.Notifications)
	if err != nil {
		return nil, err
	}

	store.BindDestinations(cfg.Notifiers)

	err = store.ConfigureReports(ctx, repo.Reports, true)
	if err != nil {
		resource.Close(store)

		return nil, err
	}

	return store, nil
}
