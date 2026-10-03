package mirror

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/resource"
)

func TestSubtreePruneDoesNotReadUnrelatedMirrorDirectory(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("permission denial requires an unprivileged user")
	}

	source := t.TempDir()
	repository := t.TempDir()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	syncer, err := Open(repository, []string{source}, matcher, "sha256")
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(syncer)

	selected := filepath.Join(source, "selected")
	unrelated := filepath.Join(repository, "unrelated")

	err = os.Mkdir(unrelated, 0)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		//nolint:gosec // Restore owner traversal on this test directory so testing can remove it.
		cleanupErr := os.Chmod(unrelated, 0o700)
		if cleanupErr != nil {
			t.Error(cleanupErr)
		}
	})

	relative := strings.TrimPrefix(selected, "/")

	err = os.MkdirAll(filepath.Join(repository, relative), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = syncer.prune(t.Context(), []string{selected}, map[string]bool{relative: true}, nil)
	if err != nil {
		t.Fatal("scoped prune entered unrelated directory", err)
	}
}

func TestGitMetadataFileDoesNotSkipPruningSiblings(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	repository := t.TempDir()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	syncer, err := Open(repository, []string{source}, matcher, "sha256")
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(syncer)

	for _, name := range []string{".git", "obsolete"} {
		err = os.WriteFile(filepath.Join(repository, name), []byte("retained"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	err = syncer.prune(t.Context(), []string{source}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = os.Stat(filepath.Join(repository, "obsolete"))
	if !os.IsNotExist(err) {
		t.Fatal("metadata file skipped stale sibling", err)
	}

	_, err = os.Stat(filepath.Join(repository, ".git"))
	if err != nil {
		t.Fatal("metadata file was removed", err)
	}
}
