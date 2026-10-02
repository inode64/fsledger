package git_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gitrepo "github.com/inode64/fsledger/internal/git"
)

func objectExists(t *testing.T, repository *gitrepo.Repository, object string) bool {
	t.Helper()

	//nolint:gosec // The test inspects only its private temporary repository.
	return exec.CommandContext(t.Context(), "git", "-C", repository.Path, "cat-file", "-e", object).Run() == nil
}

func stagedBlob(t *testing.T, repository *gitrepo.Repository, content string) string {
	t.Helper()

	err := os.WriteFile(filepath.Join(repository.Path, "db"), []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = repository.Stage(t.Context(), []string{"db"})
	if err != nil {
		t.Fatal(err)
	}

	//nolint:gosec // The test inspects only its private temporary repository.
	out, err := exec.CommandContext(t.Context(), "git", "-C", repository.Path, "rev-parse", ":db").Output()
	if err != nil {
		t.Fatal(err)
	}

	return strings.TrimSpace(string(out))
}

// Restaging without a commit leaves superseded blobs that Git's own gc keeps for weeks.
func TestPruneUnreachableKeepsHistoryAndIndex(t *testing.T) {
	t.Parallel()

	_, lookPathErr := exec.LookPath("git")
	if lookPathErr != nil {
		t.Skip("git not installed")
	}

	repository := &gitrepo.Repository{Path: t.TempDir(), Host: testHost, Timeout: 0}

	_, err := repository.Init(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	committed := stagedBlob(t, repository, "committed")

	_, err = repository.Commit(t.Context(), "Initial snapshot for testhost")
	if err != nil {
		t.Fatal(err)
	}

	superseded := stagedBlob(t, repository, "superseded")
	latest := stagedBlob(t, repository, "latest")

	err = repository.PruneUnreachable(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if objectExists(t, repository, superseded) {
		t.Fatal("superseded staged blob survived prune")
	}

	if !objectExists(t, repository, committed) || !objectExists(t, repository, latest) {
		t.Fatal("prune removed committed history or the pending index version")
	}
}
