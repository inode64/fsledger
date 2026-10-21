package aisummary

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/config"
	gitrepo "github.com/inode64/fsledger/internal/git"
)

// stagedModeChange stages a mode-only change of /config next to a new /other.
func stagedModeChange(t *testing.T) (*gitrepo.Repository, map[string]string) {
	t.Helper()
	repository, _ := stagedTexts(t, "VALUE=one\n", "VALUE=one\n")

	err := os.Chmod(filepath.Join(repository.Path, "config"), 0o700) //nolint:gosec // The executable bit is under test.
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(repository.Path, "other"), []byte("VALUE=two\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = repository.Stage(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	versions, err := repository.StagedVersions(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(versions["/config"], ":100644 100755 ") {
		t.Fatal("fixture is not a mode change", versions["/config"])
	}

	return repository, versions
}

func TestModeOnlyChangeIsNotOmitted(t *testing.T) {
	t.Parallel()
	repo, versions := stagedModeChange(t)
	manager := New(map[string]config.AIProfile{"unused": testProfile("https://unused.invalid")})
	session := testSession(t, manager, "unused")

	text, status := session.Summarize(t.Context(), repo, versions)
	if text != "" || status != "no-visible-diff" {
		t.Fatal("mode-only change reported as omitted", text, status)
	}

	session.include = []string{"/config", "/other"}

	patches, err := session.prepare(t.Context(), repo, versions)
	if err != nil {
		t.Fatal(err)
	}

	prompt := patches.bounded(config.DefaultAIDiffBytes).result()
	if !strings.Contains(prompt, " 0 archivos autorizados omitidos") || !strings.Contains(prompt, "+VALUE=two\n") {
		t.Fatal("mode-only change counted in partial summary", prompt)
	}
}
