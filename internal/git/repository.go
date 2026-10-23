// Package git encapsulates Git subprocesses without invoking a shell.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/fault"
)

// ChangeIDTrailer links a commit to the catalog operation that produced it.
const ChangeIDTrailer = "FSLedger-Change-ID:"

const queryTimeout = 30 * time.Second

// Repository is called only by its owning repository worker.
type Repository struct {
	Path string
	Host string
	// publishable is the branch name Git has already accepted for publication.
	publishable string
	Timeout     time.Duration
	// exclusive records that Init claimed the repository, so killed commands release their locks.
	exclusive bool
}

// Init initializes only an independent mirror; returns whether HEAD is absent. It claims the
// repository and removes stale locks, so the caller must hold the repository directory lock.
func (r *Repository) Init(ctx context.Context) (bool, error) {
	mkdirAllErr := os.MkdirAll(r.Path, 0o700)
	if mkdirAllErr != nil {
		return false, fmt.Errorf("create repository: %w", mkdirAllErr)
	}

	metadata := filepath.Join(r.Path, ".git")

	info, err := os.Lstat(metadata)
	switch {
	case errors.Is(err, os.ErrNotExist):
		err = r.initializeEmpty(ctx)
		if err != nil {
			return false, err
		}
	case err != nil:
		return false, fault.Wrap("inspect Git metadata", err)
	case !info.IsDir():
		return false, fault.New(".git must be a real directory")
	}

	top, err := r.run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return false, err
	}

	if strings.TrimSpace(string(top)) != r.Path {
		return false, fault.New("Git worktree is not the configured mirror")
	}

	claimErr := r.claim()
	if claimErr != nil {
		return false, claimErr
	}

	_, err = r.run(ctx, "rev-parse", "--verify", "--quiet", "HEAD")
	if err == nil {
		return false, nil
	}

	if !missingGitObject(err) {
		return false, err
	}

	ref, symbolicErr := r.run(ctx, "symbolic-ref", "--quiet", "HEAD")
	if symbolicErr != nil {
		return false, errors.Join(err, symbolicErr)
	}

	_, refErr := r.run(ctx, "show-ref", "--verify", "--quiet", strings.TrimSpace(string(ref)))
	if refErr == nil || !missingGitObject(refErr) {
		return false, errors.Join(err, refErr)
	}

	return true, nil
}

func missingGitObject(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var exitError *exec.ExitError

	return errors.As(err, &exitError) && exitError.ExitCode() == 1
}

// Commit stages the serialized worktree and avoids empty commits.
func (r *Repository) Commit(ctx context.Context, message string) (string, error) {
	runErr := r.Stage(ctx, nil)
	if runErr != nil {
		return "", runErr
	}

	return r.CommitStaged(ctx, message)
}

// Head returns the last commit identifier.
func (r *Repository) Head(ctx context.Context) (string, error) {
	out, err := r.run(ctx, "rev-parse", "--verify", "HEAD")

	return strings.TrimSpace(string(out)), err
}

// HasHistory distinguishes an unborn HEAD from a Git failure; HEAD must peel to a commit.
func (r *Repository) HasHistory(ctx context.Context) (bool, error) {
	_, err := r.run(ctx, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if missingGitObject(err) {
		return false, nil
	}

	return err == nil, err
}

// Status returns Git's stable porcelain representation.
func (r *Repository) Status(ctx context.Context) (string, error) {
	out, err := r.run(ctx, "status", "--porcelain=v1", "--untracked-files=all")

	return string(out), err
}

// CommitStaged records the current index without staging additional paths.
func (r *Repository) CommitStaged(ctx context.Context, message string) (string, error) {
	versions, err := r.StagedVersions(ctx)
	if err != nil {
		return "", err
	}

	return r.CommitVersions(ctx, message, versions)
}

// CommitVersions commits an index snapshot obtained by StagedVersions. The caller
// must serialize repository access and leave the index unchanged between calls.
func (r *Repository) CommitVersions(ctx context.Context, message string, versions map[string]string) (string, error) {
	if len(versions) == 0 {
		return "", nil
	}

	changes, err := describeVersions(versions)
	if err != nil {
		return "", err
	}

	// Stdin avoids ARG_MAX for snapshots containing many paths; no shell is used.
	_, err = r.runInput(
		ctx,
		message+"\n\nChanges (paths relative to /):\n"+changes,
		"commit",
		"--file=-",
	)
	if err != nil {
		return "", err
	}

	return r.Head(ctx)
}

func (r *Repository) run(ctx context.Context, args ...string) ([]byte, error) {
	return r.runInput(ctx, "", args...)
}

func (r *Repository) runInput(ctx context.Context, input string, args ...string) ([]byte, error) {
	git := r.command(ctx, args)
	git.cmd.Stdin = strings.NewReader(input)

	var stdout bytes.Buffer

	git.cmd.Stdout = &stdout

	err := git.start()
	if err != nil {
		return nil, err
	}

	err = git.wait()
	if err != nil {
		return nil, err
	}

	return stdout.Bytes(), nil
}

// process is a prepared Git subprocess whose timeout and error shaping are shared by every caller.
type process struct {
	// expired reports the timeout cause once the command has been reaped.
	expired func() error
	// release removes the locks a cancelled command leaves in an exclusively owned repository.
	release func() error
	cancel  context.CancelFunc
	cmd     *exec.Cmd
}

// command prepares a Git subprocess with the repository environment and timeout.
func (r *Repository) command(ctx context.Context, args []string) process {
	ctx, cancel := context.WithTimeout(ctx, r.commandTimeout(args))

	// The default core.fsync skips loose objects and references: after a crash HEAD could name lost objects.
	//nolint:gosec // Git receives explicit argument elements without a shell; paths belong to this repository.
	command := exec.CommandContext(ctx, "git", append([]string{
		"-C", r.Path, "--literal-pathspecs", "--no-optional-locks",
		"-c", "gc.autoDetach=false", "-c", "maintenance.autoDetach=false",
		"-c", "core.fsync=committed,reference", "-c", "core.fsyncMethod=batch",
	}, args...)...)
	// Keep hooks and foreground maintenance in the command's cancellation group.
	command.SysProcAttr = &syscall.SysProcAttr{}
	command.SysProcAttr.Setpgid = true
	command.Cancel = func() error { return cancelGit(command) }
	command.WaitDelay = commandWaitDelay

	command.Env = make([]string, 0, len(os.Environ()))
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_") {
			command.Env = append(command.Env, value)
		}
	}

	command.Env = append(
		command.Env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=fsledger",
		"GIT_AUTHOR_EMAIL=fsledger@"+r.Host,
		"GIT_COMMITTER_NAME=fsledger",
		"GIT_COMMITTER_EMAIL=fsledger@"+r.Host,
	)

	// Hooks and helpers may print credentials. Keep their output out of errors and persistent diagnostics.
	command.Stderr = io.Discard

	// Restores read a mirror the daemon owns; only the owner may delete locks.
	release := func() error { return nil }
	if r.exclusive {
		release = r.removeStaleLocks
	}

	return process{expired: ctx.Err, release: release, cancel: cancel, cmd: command}
}

// start launches Git; a failed launch releases the timeout immediately.
func (p process) start() error {
	err := p.cmd.Start()
	if err != nil {
		p.cancel()

		return fault.Wrap("Git command failed", err)
	}

	return nil
}

// wait collects the exit status once. Arguments may contain secrets, so the status and the
// cancellation cause are preserved without echoing them.
func (p process) wait() error {
	defer p.cancel()

	err := errors.Join(p.cmd.Wait(), p.expired())
	if p.expired() != nil {
		// Cancellation killed the whole process group before Wait reaped it: no lock holder survives.
		err = errors.Join(err, p.release())
	}

	return fault.Wrap("Git command failed", err)
}

func (r *Repository) commandTimeout(args []string) time.Duration {
	if len(args) > 0 && args[0] == "rev-parse" {
		return queryTimeout
	}

	// The zero value stays usable: a Repository built without Timeout gets the default.
	if r.Timeout > 0 {
		return r.Timeout
	}

	return config.DefaultGitTimeout
}

func (r *Repository) initializeEmpty(ctx context.Context) error {
	entries, err := os.ReadDir(r.Path)
	if err != nil {
		return fault.Wrap("read repository directory", err)
	}

	if len(entries) != 0 {
		return fault.New("refuse to initialize nonempty directory: " + r.Path)
	}

	_, err = r.run(ctx, "init", "--initial-branch=main")

	return err
}
