package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"slices"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/aide"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

const migrationOperands = 2

type aideReport struct {
	Repository    string   `json:"repository"`
	Type          string   `json:"type"`
	Catalog       string   `json:"catalog"`
	SourceSHA256  string   `json:"source_sha256"`
	IgnoredFields []string `json:"ignored_fields"`
	Imported      int64    `json:"imported"`
	Skipped       int64    `json:"skipped"`
}

func runAIDEMigration(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("migrate_aide <db_old> <repository>", flag.ContinueOnError)
	flags.SetOutput(stderr)

	configuration := flags.String("c", config.DefaultPath, "configuration path")

	operands, done, err := parseCommand(flags, args, stdout)
	if done {
		return err
	}

	if len(operands) != migrationOperands {
		return fault.New("usage: " + migrationUsage)
	}

	cfg, err := config.Load(*configuration)
	if err != nil {
		return err
	}

	return migrateAIDE(context.Background(), stdout, cfg, operands[0], operands[1])
}

func migrateAIDE(ctx context.Context, writer io.Writer, cfg *config.Config, path, name string) error {
	repository, exists := cfg.Repositories[name]
	if !exists {
		return fault.New("unknown repository: " + name)
	}

	if repository.Integrity.Reference != config.ReferenceBaseline {
		return fault.New("AIDE migration requires integrity.reference: baseline")
	}

	matcher, err := cfg.Matcher(name)
	if err != nil {
		return err
	}

	// Decode and validate the entire source before creating or opening the destination.
	spool, err := os.CreateTemp("", "fsledger-aide-*.jsonl")
	if err != nil {
		return fault.Wrap("create AIDE import spool", err)
	}
	defer removeAIDESpool(spool)

	report, err := prepareAIDE(ctx, spool, path, &repository, matcher)
	if err != nil {
		return err
	}

	if report.Imported == 0 {
		return fault.New("AIDE database has no entries selected by this repository")
	}

	err = inventoryCommand(ctx, io.Discard, cfg, commandBaseline, actionImport, name, spool.Name())
	if err != nil {
		return err
	}

	report.Repository, report.Type, report.Catalog = name, repository.Type, cfg.CatalogPath(name)

	return fault.Wrap("report AIDE migration", json.NewEncoder(writer).Encode(report))
}

func removeAIDESpool(file *os.File) {
	resource.Close(file)

	err := os.Remove(file.Name())
	if err != nil {
		// Cleanup failures must be visible without hiding the import result.
		slog.Warn("AIDE spool cleanup failed", "error", err)
	}
}

func prepareAIDE(
	ctx context.Context,
	spool io.Writer,
	path string,
	repository *config.Repository,
	matcher *exclude.Matcher,
) (aideReport, error) {
	//nolint:gosec // Explicit user-selected reference; O_NONBLOCK and fstat reject special files without blocking.
	input, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return aideReport{}, fault.Wrap("open AIDE database", err)
	}
	defer resource.Close(input)

	info, err := input.Stat()
	if err != nil {
		return aideReport{}, fault.Wrap("stat AIDE database", err)
	}

	if !info.Mode().IsRegular() {
		return aideReport{}, fault.New("AIDE database must be a regular file")
	}

	digest := sha256.New()

	reader, err := aide.New(io.TeeReader(input, digest), repository.Integrity.Hash.Algorithm)
	if err != nil {
		return aideReport{}, err
	}

	report, err := spoolAIDE(ctx, json.NewEncoder(spool), reader, repository, matcher)
	if err != nil {
		return report, err
	}

	report.SourceSHA256 = hex.EncodeToString(digest.Sum(nil))
	report.IgnoredFields = reader.IgnoredFields()

	return report, nil
}

func spoolAIDE(
	ctx context.Context,
	encoder *json.Encoder,
	reader *aide.Reader,
	repository *config.Repository,
	matcher *exclude.Matcher,
) (aideReport, error) {
	var report aideReport

	for {
		entry, err := reader.Next(ctx)
		if errors.Is(err, io.EOF) {
			return report, nil
		}

		if err != nil {
			return report, err
		}

		path := string(entry.Record.Path)

		selected := slices.ContainsFunc(
			repository.Paths,
			func(entry string) bool { return config.SourceSelects(entry, path) },
		)
		if !selected || matcher.Match(path) {
			report.Skipped++

			continue
		}

		err = entry.Validate(repository.Integrity)
		if err != nil {
			return report, err
		}

		err = encoder.Encode(entry.Record)
		if err != nil {
			return report, fault.Wrap("spool AIDE observation", err)
		}

		report.Imported++
	}
}
