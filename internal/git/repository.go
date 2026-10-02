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
}

// Init initializes only an independent mirror; returns whether HEAD is absent.
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
	case !info.IsDir() || info.Mode()&os.ModeSymlink != 0:
		return false, fault.New(".git must be a real directory")
	}

	top, err := r.run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return false, err
	}

	if strings.TrimSpace(string(top)) != r.Path {
		return false, fault.New("Git worktree is not the configured mirror")
	}

	attributeErr := r.preserveContent()
	if attributeErr != nil {
		return false, attributeErr
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
	ctx, cancel := context.WithTimeout(ctx, r.commandTimeout(args))
	defer cancel()

	//nolint:gosec // Git receives explicit argument elements without a shell; paths belong to this repository.
	command := exec.CommandContext(ctx, "git", append([]string{
		"-C", r.Path, "--literal-pathspecs", "--no-optional-locks",
		"-c", "gc.autoDetach=false", "-c", "maintenance.autoDetach=false",
	}, args...)...)
	// Keep hooks and foreground maintenance in the command's cancellation group.
	command.SysProcAttr = &syscall.SysProcAttr{}
	command.SysProcAttr.Setpgid = true
	command.Cancel = func() error { return cancelGit(command) }
	command.WaitDelay = commandWaitDelay

	command.Stdin = strings.NewReader(input)

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

	var stdout bytes.Buffer

	command.Stdout = &stdout

	// Hooks and helpers may print credentials. Keep their output out of errors and persistent diagnostics.
	command.Stderr = io.Discard

	err := command.Run()
	if err != nil {
		// Arguments may also contain secrets. Preserve exit status and cancellation without echoing them.
		return nil, fault.Wrap("Git command failed", errors.Join(err, ctx.Err()))
	}

	return stdout.Bytes(), nil
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
