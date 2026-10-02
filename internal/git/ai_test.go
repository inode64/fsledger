package git_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/redact"
)

func TestStagedTextUsesExactBlobsAndOmitsUnsafeTypes(t *testing.T) {
	t.Parallel()
	repo := newRepository(t)
	writeRepositoryFile(t, repo, "changed", "before")
	writeRepositoryFile(t, repo, "deleted", "deleted-content")

	_, err := repo.Commit(t.Context(), "before AI test")
	if err != nil {
		t.Fatal(err)
	}

	writeRepositoryFile(t, repo, "changed", "staged")
	writeRepositoryFile(t, repo, "added", "added-content")
	writeRepositoryFile(t, repo, "binary", "secret\x00binary")
	writeRepositoryFile(t, repo, "large", strings.Repeat("s", redact.MaxTextBytes+1))

	err = os.Remove(filepath.Join(repo.Path, "deleted"))
	if err != nil {
		t.Fatal(err)
	}

	err = os.Symlink("/outside-secret", filepath.Join(repo.Path, "link"))
	if err != nil {
		t.Fatal(err)
	}

	err = repo.Stage(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	versions, err := repo.StagedVersions(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	writeRepositoryFile(t, repo, "changed", "unstaged-newer-content")

	for path, expected := range map[string][2]string{
		"/changed": {"before", "staged"},
		"/deleted": {"deleted-content", ""},
		"/added":   {"", "added-content"},
	} {
		before, after, eligible, readErr := repo.StagedText(t.Context(), versions[path])
		if readErr != nil || !eligible || before != expected[0] || after != expected[1] {
			t.Fatal(path, before, after, eligible, readErr)
		}
	}

	for _, path := range []string{"/binary", "/large", "/link"} {
		_, _, eligible, readErr := repo.StagedText(t.Context(), versions[path])
		if readErr != nil || eligible {
			t.Fatal("unsafe blob accepted", path, readErr)
		}
	}
}
