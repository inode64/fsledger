package git_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	gitrepo "github.com/inode64/fsledger/internal/git"
)

const (
	directoryType = "directory"
	testHost      = "testhost"
	testFile      = "file"
)

func TestCommitTimeoutStopsHookGroup(t *testing.T) {
	t.Parallel()
	repository := newRepository(t)
	writeRepositoryFile(t, repository, testFile, "initial")
	commitPaths(t, repository, testFile)
	writeRepositoryFile(t, repository, testFile, "pending")
	hook := filepath.Join(repository.Path, ".git", "hooks", "pre-commit")
	//nolint:gosec // Executable hook is confined to this private test repository.
	err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf started > .git/hook-started\nsleep 30\n"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	repository.Timeout = time.Second
	started := time.Now()

	_, err = repository.CommitPaths(t.Context(), "timeout", []string{testFile})
	if err == nil {
		t.Fatal("blocking hook ignored timeout")
	}

	if time.Since(started) > 5*time.Second {
		t.Fatal("Git cancellation waited for a surviving hook")
	}

	_, statErr := os.Stat(filepath.Join(repository.Path, ".git", "hook-started"))
	if statErr != nil {
		t.Fatal("Git did not reach the hook", statErr)
	}

	if got := repositoryOutput(t, repository, "show", "HEAD:file"); got != "initial" {
		t.Fatal("timed out commit changed HEAD")
	}
}

func newRepository(t *testing.T) *gitrepo.Repository {
	t.Helper()
	repository := &gitrepo.Repository{Path: t.TempDir(), Host: testHost, Timeout: 0}

	_, err := repository.Init(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	return repository
}

func writeRepositoryFile(t *testing.T, repository *gitrepo.Repository, path, content string) {
	t.Helper()

	err := os.MkdirAll(filepath.Dir(filepath.Join(repository.Path, path)), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(repository.Path, path), []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func repositoryOutput(t *testing.T, repository *gitrepo.Repository, args ...string) string {
	t.Helper()
	//nolint:gosec // Explicit Git arguments and a private temporary repository, without a shell.
	output, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", repository.Path}, args...)...).Output()
	if err != nil {
		t.Fatal(err)
	}

	return string(output)
}

func commitPaths(t *testing.T, repository *gitrepo.Repository, paths ...string) {
	t.Helper()

	_, err := repository.CommitPaths(t.Context(), "affected paths", paths)
	if err != nil {
		t.Fatal(err)
	}
}

func TestIncrementalStagingUsesLiteralPaths(t *testing.T) {
	t.Parallel()
	repository := newRepository(t)
	writeRepositoryFile(t, repository, "other", "original")
	commitPaths(t, repository, "other")
	writeRepositoryFile(t, repository, "other", "unrelated change")
	writeRepositoryFile(t, repository, ".gitignore", "*\n")

	paths := []string{".gitignore", "-option", ":(glob)*", "wild[1]", "line\nbreak", "quoted\"file"}
	for _, path := range paths[1:] {
		writeRepositoryFile(t, repository, path, path)
	}

	commitPaths(t, repository, paths...)

	for _, path := range paths[1:] {
		if got := repositoryOutput(t, repository, "show", "HEAD:"+path); got != path {
			t.Fatalf("literal path %q: got %q", path, got)
		}
	}

	if got := repositoryOutput(t, repository, "show", "HEAD:other"); got != "original" {
		t.Fatalf("included unrelated path: %q", got)
	}
}

func TestIncrementalStagingRemovesSubtreesAndIgnoresTransientFiles(t *testing.T) {
	t.Parallel()
	repository := newRepository(t)
	writeRepositoryFile(t, repository, "old/nested/file", "old")
	commitPaths(t, repository, "old")

	err := os.Rename(filepath.Join(repository.Path, "old"), filepath.Join(repository.Path, "new"))
	if err != nil {
		t.Fatal(err)
	}

	commitPaths(t, repository, "old", "new", "never-existed")

	if got := repositoryOutput(t, repository, "ls-tree", "-r", "--name-only", "HEAD"); got != "new/nested/file\n" {
		t.Fatalf("wrong renamed tree: %q", got)
	}

	head, err := repository.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	commitPaths(t, repository, "never-existed")

	if got := repositoryOutput(t, repository, "rev-parse", "HEAD"); got != head+"\n" {
		t.Fatal("transient file created a commit")
	}
}

//nolint:gocognit // Ordered real-Git transitions verify both directions of type replacement.
func TestIncrementalStagingTypeReplacement(t *testing.T) {
	t.Parallel()

	for _, original := range []string{testFile, directoryType, "symlink"} {
		t.Run(original, func(t *testing.T) {
			t.Parallel()
			repository := newRepository(t)
			target := filepath.Join(repository.Path, "entry")

			switch original {
			case testFile:
				writeRepositoryFile(t, repository, "entry", "old")
			case directoryType:
				writeRepositoryFile(t, repository, "entry/old", "old")
			case "symlink":
				err := os.Symlink("nowhere", target)
				if err != nil {
					t.Fatal(err)
				}
			}

			commitPaths(t, repository, "entry")

			err := os.RemoveAll(target)
			if err != nil {
				t.Fatal(err)
			}

			path := "entry/new"
			if original == directoryType {
				path = "entry"
			}

			writeRepositoryFile(t, repository, path, "replacement")
			commitPaths(t, repository, path)

			if got := repositoryOutput(t, repository, "ls-tree", "-r", "--name-only", "HEAD"); got != path+"\n" {
				t.Fatalf("wrong replacement tree: %q", got)
			}
		})
	}
}
