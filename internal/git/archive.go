package git

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/inode64/fsledger/internal/fault"
)

// CommitBefore returns the last commit reachable from HEAD whose committer date is at or before when.
func (r *Repository) CommitBefore(ctx context.Context, when time.Time) (string, time.Time, error) {
	before := "--before=@" + strconv.FormatInt(when.Unix(), 10)

	out, err := r.run(ctx, "log", "-1", "--format=%H %ct", before, "HEAD")
	if err != nil {
		return "", time.Time{}, err
	}

	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", time.Time{}, fault.New("no commit at or before " + when.Format(time.RFC3339))
	}

	commit, stamp, found := strings.Cut(line, " ")
	if !found {
		return "", time.Time{}, fault.New("unexpected Git log output")
	}

	seconds, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return "", time.Time{}, fault.Wrap("parse commit date", err)
	}

	return commit, time.Unix(seconds, 0), nil
}

// Archive streams a tar of path at commit. Close reports the subprocess outcome, so a path
// absent from the commit surfaces there once the (empty) stream has been consumed.
func (r *Repository) Archive(ctx context.Context, commit, path string) (io.ReadCloser, error) {
	git := r.command(ctx, []string{"archive", "--format=tar", commit, "--", path})

	stdout, err := git.cmd.StdoutPipe()
	if err != nil {
		git.cancel()

		return nil, fault.Wrap("open Git archive pipe", err)
	}

	err = git.start()
	if err != nil {
		return nil, err
	}

	return &archiveStream{ReadCloser: stdout, process: git}, nil
}

type archiveStream struct {
	io.ReadCloser

	process process
}

// Close drains the stream so Git can exit normally, then reports its exit status.
func (s *archiveStream) Close() error {
	_, drainErr := io.Copy(io.Discard, s.ReadCloser)

	return errors.Join(s.process.wait(), fault.Wrap("drain Git archive", drainErr))
}
