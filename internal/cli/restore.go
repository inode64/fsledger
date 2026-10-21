package cli

import (
	"archive/tar"
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/fault"
	gitrepo "github.com/inode64/fsledger/internal/git"
	"github.com/inode64/fsledger/internal/mirror"
	"github.com/inode64/fsledger/internal/restore"
)

const (
	restoreUsage = "fsledger " + commandRestore +
		" --repository NAME --date DATE [--overwrite off|always|newer] [--dry-run] " +
		"SOURCE [DESTINATION] [-c configuration]"
	restoreMaxOperands = 2
	dateLayoutMinute   = "2006-01-02 15:04"
	shortCommit        = 7
	archiveBufferSize  = 64 * 1024
	restoreNotice      = "Ownership, ACLs and extended attributes are not stored in Git mirrors and were not restored"
)

type restoreRequest struct {
	when        time.Time
	repository  string
	source      string
	destination string
	policy      restore.Policy
	dryRun      bool
}

func runRestore(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet(commandRestore, flag.ContinueOnError)
	flags.SetOutput(stderr)

	configuration := flags.String("c", config.DefaultPath, "configuration path")
	repository := flags.String("repository", "", "repository name")
	date := flags.String("date", "", "restore the last version committed at or before this date")
	overwrite := flags.String("overwrite", "off", "overwrite policy for existing entries: off, always or newer")
	dryRun := flags.Bool("dry-run", false, "print the planned actions without writing")

	operands, done, err := parseCommand(flags, args, stdout)
	if done {
		return err
	}

	if len(operands) == 0 || len(operands) > restoreMaxOperands || *repository == "" || *date == "" {
		return fault.New("usage: " + restoreUsage)
	}

	policy, err := restore.ParsePolicy(*overwrite)
	if err != nil {
		return err
	}

	when, err := parseRestoreDate(*date, time.Local) //nolint:gosmopolitan // Operators type local dates.
	if err != nil {
		return err
	}

	source, destination, err := restorePaths(operands)
	if err != nil {
		return err
	}

	cfg, err := config.Load(*configuration)
	if err != nil {
		return err
	}

	return restoreFromRepository(context.Background(), stdout, cfg, restoreRequest{
		when: when, repository: *repository, source: source, destination: destination, policy: policy, dryRun: *dryRun,
	})
}

// parseRestoreDate accepts local dates with optional time, or RFC 3339. A bare date means its end.
func parseRestoreDate(value string, location *time.Location) (time.Time, error) {
	when, err := time.Parse(time.RFC3339, value)
	if err == nil {
		return when, nil
	}

	for _, layout := range []string{time.DateTime, dateLayoutMinute, time.DateOnly} {
		when, err = time.ParseInLocation(layout, value, location)
		if err != nil {
			continue
		}

		if layout == time.DateOnly {
			when = when.AddDate(0, 0, 1).Add(-time.Second)
		}

		return when, nil
	}

	return time.Time{}, fault.New("--date must be YYYY-MM-DD, YYYY-MM-DD HH:MM[:SS] or RFC 3339: " + value)
}

// restorePaths returns the archived path and the absolute destination; the destination defaults
// to the source's original location.
func restorePaths(operands []string) (string, string, error) {
	source, err := mirror.Relative(path.Clean("/" + operands[0]))
	if err != nil {
		return "", "", err
	}

	if len(operands) < restoreMaxOperands {
		return source, "/" + source, nil
	}

	destination := path.Clean(operands[1])

	return source, destination, config.CleanAbsolute(destination)
}

func restoreFromRepository(ctx context.Context, writer io.Writer, cfg *config.Config, request restoreRequest) error {
	backend, err := restoreBackend(ctx, cfg, request.repository)
	if err != nil {
		return err
	}

	commit, committed, err := backend.CommitBefore(ctx, request.when)
	if err != nil {
		return err
	}

	short := commit[:shortCommit]

	archive, err := backend.Archive(ctx, commit, request.source)
	if err != nil {
		return err
	}

	summary, err := restore.Apply(ctx, tar.NewReader(bufio.NewReaderSize(archive, archiveBufferSize)), restore.Options{
		Committed: committed, Output: writer, Destination: request.destination, Prefix: request.source,
		Policy: request.policy, DryRun: request.dryRun,
	})

	closeErr := archive.Close()

	switch {
	case errors.Is(err, restore.ErrSourceMissing):
		return fault.New(request.source + " does not exist in commit " + short)
	case err != nil || closeErr != nil:
		return errors.Join(err, closeErr)
	}

	_, err = fmt.Fprintf(writer, "Restored %d, skipped %d, failed %d from %s (%s)\n%s\n",
		summary.Restored, summary.Skipped, summary.Failed, short, committed.Format(time.RFC3339), restoreNotice)
	if err != nil {
		return fault.Wrap("write restore summary", err)
	}

	if summary.Failed > 0 {
		return fault.New("restore completed with failures")
	}

	return nil
}

// restoreBackend selects an existing Git mirror with at least one commit; Git's own failure on a
// missing HEAD would be opaque because its stderr is discarded.
func restoreBackend(ctx context.Context, cfg *config.Config, name string) (*gitrepo.Repository, error) {
	repo, err := selectRepository(cfg, name)
	if err != nil {
		return nil, err
	}

	if repo.Type != config.RepositoryGit {
		return nil, fault.New("restore requires a git repository; " + name + " is " + repo.Type)
	}

	backend := gitBackend(cfg, name)

	info, err := os.Lstat(filepath.Join(backend.Path, ".git"))
	if err != nil || !info.IsDir() {
		return nil, fault.New("repository has no Git mirror yet: " + name)
	}

	history, err := backend.HasHistory(ctx)
	if err != nil {
		return nil, err
	}

	if !history {
		return nil, fault.New("repository has no recorded history yet: " + name)
	}

	return backend, nil
}
