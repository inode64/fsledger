package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"go.yaml.in/yaml/v3"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/fault"
	gitrepo "github.com/inode64/fsledger/internal/git"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/notify"
	"github.com/inode64/fsledger/internal/resource"
)

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

func inventoryCommand(
	ctx context.Context,
	writer io.Writer,
	cfg *config.Config,
	command, action, name, change string,
) error {
	repo, exists := cfg.Repositories[name]
	if !exists {
		return fault.New("select an existing repository with --repository")
	}

	var err error
	if command == commandBaseline && action == actionImport {
		err = cfg.CheckStoragePaths()
	} else {
		err = cfg.CheckPaths()
	}

	if err != nil {
		return err
	}

	root := cfg.RepositoryPath(name)

	lock, err := resource.LockDirectory(root)
	if err != nil {
		return err
	}
	defer resource.Close(lock)

	err = config.CheckRepositoryStorage(root, repo.Type)
	if err != nil {
		return err
	}

	err = initializeInventoryGit(ctx, cfg, name)
	if err != nil {
		return err
	}

	store, err := catalog.OpenRepository(ctx, cfg, name)
	if err != nil {
		return err
	}
	defer resource.Close(store)

	return executeInventory(ctx, writer, cfg, store, command, action, name, change)
}

func initializeInventoryGit(ctx context.Context, cfg *config.Config, name string) error {
	repo := cfg.Repositories[name]
	if repo.Type != config.RepositoryGit {
		return nil
	}

	backend := gitrepo.Repository{
		Path:    cfg.RepositoryPath(name),
		Host:    cfg.Server.Name,
		Timeout: repo.Storage.Git.Timeout,
	}
	_, err := backend.Init(ctx)

	return err
}

func previewReport(ctx context.Context, writer io.Writer, cfg *config.Config, name string) error {
	if _, exists := cfg.Repositories[name]; !exists {
		return fault.New("select an existing repository with --repository")
	}

	err := cfg.CheckStoragePaths()
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

func executeInventory(
	ctx context.Context,
	writer io.Writer,
	cfg *config.Config,
	store *catalog.Store,
	command, action, name, change string,
) error {
	var err error

	switch command {
	case "changes":
		return store.Changes(ctx, changesDisplayLimit, func(id string, path []byte, kind string, fields []byte) error {
			_, writeErr := fmt.Fprintf(writer, "%s %s %q %s\n", id, kind, path, fields)

			return fault.Wrap("write change", writeErr)
		})
	case commandBaseline:
		if action == actionImport {
			return importReference(ctx, writer, store, change)
		}

		if action == "accept" {
			if change == "" {
				return fault.New("baseline accept requires --change")
			}

			err = store.Accept(ctx, change)
			if err != nil {
				return err
			}

			_, err = fmt.Fprintln(writer, "Recorded change accepted:", change)

			return fault.Wrap("write approval", err)
		}

		if action != "init" && action != actionReplace {
			return fault.New("usage: baseline init|accept|replace --repository NAME")
		}
	}

	return verifyInventory(ctx, writer, cfg, store, command, action, name)
}

func verifyInventory(
	ctx context.Context,
	writer io.Writer,
	cfg *config.Config,
	store *catalog.Store,
	command, action, name string,
) error {
	repo := cfg.Repositories[name]

	sources := cfg.ResolveRepository(name)
	if sources.Incomplete {
		return fault.New("refuse manual verification with incomplete source selection")
	}

	matcher, err := cfg.Matcher(name)
	if err != nil {
		return err
	}

	scanner := integrity.NewScanner(cfg.Scan.Workers, cfg.Scan.MaxConcurrent)
	replace := command == commandBaseline && action == actionReplace

	var result catalog.Result

	if replace {
		result, err = store.ScanAndReplaceBaseline(ctx, scanner, sources.Roots, matcher)
	} else {
		result, err = store.Reconcile(ctx, scanner, sources.Roots, matcher, true, "manual verification", "")
	}

	if err != nil {
		return err
	}

	if command == commandBaseline && !replace {
		err = store.InitBaseline(ctx)
		if err != nil {
			return err
		}
	}

	err = store.ClearOperation(ctx)
	if err != nil {
		return err
	}

	return reportVerification(
		ctx,
		writer,
		store,
		result,
		command == commandVerify && repo.Integrity.Reference == commandBaseline,
	)
}

func reportVerification(
	ctx context.Context,
	writer io.Writer,
	store *catalog.Store,
	result catalog.Result,
	requireBaseline bool,
) error {
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

	if requireBaseline && !stats.BaselineReady {
		return fault.New("no approved baseline; use baseline init after reviewing the sources")
	}

	if stats.Violations > 0 {
		return fault.New("integrity violations detected")
	}

	return nil
}

const changesDisplayLimit = 100

const actionReplace = "replace"

func importReference(ctx context.Context, writer io.Writer, store *catalog.Store, path string) error {
	if path == "" {
		return fault.New("baseline import requires --input FILE")
	}

	//nolint:gosec // --input explicitly selects the reviewed reference file to import.
	input, err := os.Open(path)
	if err != nil {
		return fault.Wrap("open reviewed reference", err)
	}
	defer resource.Close(input)

	err = store.ImportBaseline(ctx, input)
	if err != nil {
		return err
	}

	stats, err := store.Stats(ctx)
	if err != nil {
		return err
	}

	return fault.Wrap("report reference import", json.NewEncoder(writer).Encode(stats))
}
