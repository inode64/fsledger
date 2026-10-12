package git_test

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	gitrepo "github.com/inode64/fsledger/internal/git"
)

// commitDated records every working tree change with a fixed committer date, bypassing the
// repository wrapper because it strips GIT_* variables from the environment.
func commitDated(t *testing.T, repository *gitrepo.Repository, when time.Time, message string) string {
	t.Helper()

	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", message}} {
		//nolint:gosec // Explicit Git arguments and a private temporary repository, without a shell.
		command := exec.CommandContext(t.Context(), "git", append([]string{"-C", repository.Path}, args...)...)
		command.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.org",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.org",
			"GIT_AUTHOR_DATE="+when.Format(time.RFC3339),
			"GIT_COMMITTER_DATE="+when.Format(time.RFC3339),
		)

		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatal(string(output), err)
		}
	}

	return strings.TrimSpace(repositoryOutput(t, repository, "rev-parse", "HEAD"))
}

func TestCommitBeforeSelectsLatestCommitAtOrBeforeDate(t *testing.T) {
	t.Parallel()

	repository := newRepository(t)
	first := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	second := first.Add(48 * time.Hour)

	writeRepositoryFile(t, repository, "etc/motd", "v1")
	firstHash := commitDated(t, repository, first, "first")
	writeRepositoryFile(t, repository, "etc/motd", "v2")
	secondHash := commitDated(t, repository, second, "second")

	for _, scenario := range []struct {
		when      time.Time
		committed time.Time
		name      string
		want      string
	}{
		{name: "exact first", when: first, want: firstHash, committed: first},
		{name: "between", when: first.Add(time.Hour), want: firstHash, committed: first},
		{name: "after second", when: second.Add(time.Hour), want: secondHash, committed: second},
	} {
		commit, committed, err := repository.CommitBefore(t.Context(), scenario.when)
		if err != nil || commit != scenario.want || !committed.Equal(scenario.committed) {
			t.Fatalf("%s: %q %v %v", scenario.name, commit, committed, err)
		}
	}

	_, _, err := repository.CommitBefore(t.Context(), first.Add(-time.Second))
	if err == nil || !strings.Contains(err.Error(), "no commit at or before") {
		t.Fatalf("expected a missing commit error, got %v", err)
	}
}

func TestArchiveStreamsTreeAndReportsMissingPath(t *testing.T) {
	t.Parallel()

	repository := newRepository(t)
	writeRepositoryFile(t, repository, "etc/ssh/sshd_config", "config")
	commit := commitDated(t, repository, time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC), "tree")

	archive, err := repository.Archive(t.Context(), commit, "etc/ssh")
	if err != nil {
		t.Fatal(err)
	}

	var names []string

	reader := tar.NewReader(archive)

	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}

		if nextErr != nil {
			t.Fatal(nextErr)
		}

		// Git prefixes the stream with a pax global header carrying the commit identifier.
		if header.Typeflag != tar.TypeXGlobalHeader {
			names = append(names, header.Name)
		}
	}

	err = archive.Close()
	if err != nil || strings.Join(names, " ") != "etc/ etc/ssh/ etc/ssh/sshd_config" {
		t.Fatal(names, err)
	}

	missing, err := repository.Archive(t.Context(), commit, "etc/missing")
	if err != nil {
		t.Fatal(err)
	}

	content, err := io.ReadAll(missing)
	if err != nil || len(content) != 0 {
		t.Fatal(len(content), err)
	}

	err = missing.Close()
	if err == nil {
		t.Fatal("missing path did not fail")
	}
}
