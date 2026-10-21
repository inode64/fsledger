package git_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	gitrepo "github.com/inode64/fsledger/internal/git"
)

const indexLock = "index.lock"

func plantLock(t *testing.T, repository *gitrepo.Repository, name string) string {
	t.Helper()

	path := filepath.Join(repository.Path, ".git", name)

	err := os.WriteFile(path, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

func TestInitRemovesOrphanedLocks(t *testing.T) {
	t.Parallel()
	repository := newRepository(t)
	writeRepositoryFile(t, repository, testFile, "initial")
	commitPaths(t, repository, testFile)

	// A crashed daemon or an OOM-killed Git leaves these behind; Git never expires them.
	names := []string{
		indexLock,
		"HEAD.lock",
		filepath.Join("refs", "heads", "main.lock"),
		filepath.Join("objects", "maintenance.lock"),
	}

	locks := make([]string, 0, len(names))
	for _, name := range names {
		locks = append(locks, plantLock(t, repository, name))
	}

	restarted := &gitrepo.Repository{Path: repository.Path, Host: testHost, Timeout: 0}

	_, err := restarted.Init(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	writeRepositoryFile(t, restarted, testFile, "later")
	commitPaths(t, restarted, testFile)

	for _, lock := range locks {
		_, err = os.Lstat(lock)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal("orphaned lock survived Init", lock, err)
		}
	}
}

func TestTimeoutReleasesKilledCommandLocks(t *testing.T) {
	t.Parallel()
	repository := newRepository(t)
	writeRepositoryFile(t, repository, testFile, "initial")
	commitPaths(t, repository, testFile)
	writeRepositoryFile(t, repository, testFile, "pending")
	// The prepared transaction holds HEAD.lock and the branch lock until the hook returns.
	hook := filepath.Join(repository.Path, ".git", "hooks", "reference-transaction")
	//nolint:gosec // Executable hook is confined to this private test repository.
	err := os.WriteFile(hook, []byte("#!/bin/sh\n[ \"$1\" != prepared ] || sleep 30\n"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	repository.Timeout = time.Second

	_, err = repository.CommitPaths(t.Context(), "timeout", []string{testFile})
	if err == nil {
		t.Fatal("blocking reference transaction ignored timeout")
	}

	err = os.Remove(hook)
	if err != nil {
		t.Fatal(err)
	}

	repository.Timeout = 0
	commitPaths(t, repository, testFile)

	if got := repositoryOutput(t, repository, "show", "HEAD:file"); got != "pending" {
		t.Fatal("commit after a timeout did not record the change", got)
	}
}

// Restores read the mirror while the daemon writes it, so their cancellation must not touch its locks.
func TestUnclaimedRepositoryKeepsLocks(t *testing.T) {
	t.Parallel()
	owner := newRepository(t)
	writeRepositoryFile(t, owner, testFile, "initial")
	commitPaths(t, owner, testFile)

	head, err := owner.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	lock := plantLock(t, owner, indexLock)
	reader := &gitrepo.Repository{Path: owner.Path, Host: testHost, Timeout: 0}
	ctx, cancel := context.WithCancel(t.Context())

	stream, err := reader.Archive(ctx, head, testFile)
	if err != nil {
		cancel()
		t.Fatal(err)
	}

	cancel()

	err = stream.Close()
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled archive was not reported", err)
	}

	_, err = os.Lstat(lock)
	if err != nil {
		t.Fatal("cancelled reader removed a lock the owner may hold", err)
	}
}
