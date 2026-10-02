package git_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCommitVersionsPreservesSnapshotAndPathEvidence(t *testing.T) {
	t.Parallel()
	repo := newRepository(t)
	writeRepositoryFile(t, repo, "deleted", "old")

	_, err := repo.Commit(t.Context(), "initial")
	if err != nil {
		t.Fatal(err)
	}

	err = os.Remove(filepath.Join(repo.Path, "deleted"))
	if err != nil {
		t.Fatal(err)
	}

	name := "line\nwith\tquote\"\xff"
	writeRepositoryFile(t, repo, name, "staged bytes")

	err = repo.Stage(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	versions, err := repo.StagedVersions(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	writeRepositoryFile(t, repo, name, "new unstaged bytes")

	hash, err := repo.CommitVersions(t.Context(), "snapshot", versions)
	if err != nil || hash == "" {
		t.Fatalf("commit snapshot: %q %v", hash, err)
	}

	if got := repositoryOutput(t, repo, "show", "HEAD:"+name); got != "staged bytes" {
		t.Fatalf("committed %q", got)
	}

	message := repositoryOutput(t, repo, "log", "-1", "--format=%B")
	for _, evidence := range []string{"D\t" + strconv.Quote("deleted"), "A\t" + strconv.Quote(name)} {
		if !strings.Contains(message, evidence) {
			t.Fatalf("missing %q in %q", evidence, message)
		}
	}
}
