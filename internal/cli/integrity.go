package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/mountinfo"
	"github.com/inode64/fsledger/internal/notify"
	"github.com/inode64/fsledger/internal/resource"
)

// ErrChanges reports a manual verification that recorded changes. Its output is complete, so the
// command exits with status 2 rather than as a failure.
var ErrChanges = errors.New("verify recorded changes")

// printCheck reports absent sources without failing: a shared list names paths of other hosts.
func printCheck(writer io.Writer, cfg *config.Config) error {
	present, absent := 0, 0

	var skipped []string

	resolved := cfg.ResolveSources()
	for _, name := range cfg.Names() {
		sources := resolved[name]
		present += len(sources.Roots)
		absent += len(sources.Missing)

		for _, reason := range sources.Skipped {
			skipped = append(skipped, name+": "+reason)
		}
	}

	_, err := fmt.Fprintf(writer, "Configuration valid: %d repositories; sources: %d present, %d absent, %d skipped\n",
		len(cfg.Repositories), present, absent, len(skipped))
	for _, reason := range skipped {
		if err == nil {
			_, err = fmt.Fprintln(writer, "Skipped source:", reason)
		}
	}

	return fault.Wrap("write check result", err)
}

func printEffective(writer io.Writer, cfg *config.Config) error {
	notifiers := make(map[string]config.Notifier, len(cfg.Notifiers))
	for name, notifier := range cfg.Notifiers {
		if notifier.DSN != "" {
			notifier.DSN = "[redacted]"
		}

		if notifier.URL != "" {
			notifier.URL = "[redacted]"
		}

		notifiers[name] = notifier
	}

	report := map[string]any{
		"sources": cfg.ResolveSources(),
		"paths":   cfg.Paths, "scan": cfg.Scan, "ai": cfg.AI,
		"repositories": cfg.Repositories, "notifiers": notifiers, "exclude": cfg.Exclude,
		"logging": cfg.Logging, "server": cfg.Server, "notifications": cfg.Notifications,
		"reports": cfg.Reports,
	}

	return fault.Wrap("write effective configuration", yaml.NewEncoder(writer).Encode(report))
}

// verifyCommand runs a manual full scan with the repository lock, so the daemon must be stopped.
func verifyCommand(ctx context.Context, writer io.Writer, cfg *config.Config, name string) error {
	_, err := selectRepository(cfg, name)
	if err != nil {
		return err
	}

	err = cfg.CheckPaths()
	if err != nil {
		return err
	}

	return withRepositoryStore(ctx, cfg, name,
		func() error { return initializeInventoryGit(ctx, cfg, name) },
		func(store *catalog.Store) error { return verifyInventory(ctx, writer, cfg, store, name) })
}

// withRepositoryStore opens a repository's catalog under the repository lock, so the daemon must be
// stopped. prepare runs once the lock is held and the storage checked, before the catalog opens.
func withRepositoryStore(
	ctx context.Context,
	cfg *config.Config,
	name string,
	prepare func() error,
	use func(*catalog.Store) error,
) error {
	root := cfg.RepositoryPath(name)

	lock, err := resource.LockDirectory(root)
	if err != nil {
		return err
	}
	defer resource.Close(lock)

	err = config.CheckRepositoryStorage(root, cfg.Repositories[name].Type)
	if err != nil {
		return err
	}

	err = prepare()
	if err != nil {
		return err
	}

	store, err := catalog.OpenRepository(ctx, cfg, name)
	if err != nil {
		return err
	}
	defer resource.Close(store)

	return use(store)
}

// requireCatalog refuses to read a repository that has no catalog yet instead of creating an empty one.
func requireCatalog(cfg *config.Config, name string) error {
	info, err := os.Lstat(cfg.CatalogPath(name))
	if err != nil || !info.IsDir() {
		return fault.New("repository has no catalog: " + name)
	}

	return nil
}

func initializeInventoryGit(ctx context.Context, cfg *config.Config, name string) error {
	if cfg.Repositories[name].Type != config.RepositoryGit {
		return nil
	}

	_, err := gitBackend(cfg, name).Init(ctx)

	return err
}

func previewReport(ctx context.Context, writer io.Writer, cfg *config.Config, name string) error {
	_, err := selectRepository(cfg, name)
	if err != nil {
		return err
	}

	err = cfg.CheckStoragePaths()
	if err != nil {
		return err
	}

	message, err := catalog.ReadReport(ctx, cfg.CatalogPath(name), name, cfg.Server.Name)
	if err != nil {
		return err
	}

	subject, body, err := notify.ReportText(message)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(writer, "%s\n\n%s", subject, body)

	return fault.Wrap("write report preview", err)
}

// clearOutbox discards the notifications queued by one repository. It takes the repository lock, so the
// daemon must be stopped, and never creates a catalog: a repository without one has nothing queued.
func clearOutbox(ctx context.Context, writer io.Writer, cfg *config.Config, name string) error {
	_, err := selectRepository(cfg, name)
	if err != nil {
		return err
	}

	err = cfg.CheckStoragePaths()
	if err != nil {
		return err
	}

	return withRepositoryStore(ctx, cfg, name,
		func() error { return requireCatalog(cfg, name) },
		func(store *catalog.Store) error {
			discarded, discardErr := store.DiscardPending(ctx)
			if discardErr != nil {
				return discardErr
			}

			_, writeErr := fmt.Fprintf(writer, "Discarded %d pending notifications from %s\n", discarded, name)

			return fault.Wrap("write outbox result", writeErr)
		})
}

func verifyInventory(
	ctx context.Context,
	writer io.Writer,
	cfg *config.Config,
	store *catalog.Store,
	name string,
) error {
	sources := cfg.ResolveRepository(name)
	if sources.Incomplete {
		// The skipped sources say which one must become readable again.
		return fault.New("refuse manual verification with incomplete source selection: " +
			strings.Join(sources.Skipped, "; "))
	}

	matcher, err := cfg.Matcher(name)
	if err != nil {
		return err
	}

	scanner := integrity.NewScanner(cfg.Scan.Workers, cfg.Scan.MaxConcurrent)
	// A source on a recorded mount point that is no longer mounted is unavailable, not an empty tree.
	scanner.ExpectMounts(mountinfo.Points(store.Mounts()))

	// Mirror refreshes the daemon left pending stay for it: verify cannot copy them.
	result, err := store.Reconcile(ctx, scanner, sources.Roots, matcher, true, "manual verification", "")
	if err != nil {
		return err
	}

	stats, err := store.Stats(ctx)
	if err != nil {
		return err
	}

	err = json.NewEncoder(writer).Encode(struct {
		Result catalog.Result `json:"result"`
		Stats  catalog.Stats  `json:"status"`
	}{result, stats})
	if err != nil {
		return fault.Wrap("write verification", err)
	}

	if result.Changed > 0 {
		return ErrChanges
	}

	return nil
}
