package git_test

import (
	"archive/tar"
	"errors"
	"io"
	"maps"
	"path/filepath"
	"testing"

	gitrepo "github.com/inode64/fsledger/internal/git"
)

// legacyAttributes is the info/attributes block of releases that left the export rules set.
const legacyAttributes = `
# fsledger: retain source bytes; override content conversions from monitored attributes.
* -text -crlf -filter -ident -working-tree-encoding
`

// archivedFiles returns the regular files in the archive of path at commit, keyed by name.
func archivedFiles(t *testing.T, repository *gitrepo.Repository, commit, path string) map[string]string {
	t.Helper()

	archive, err := repository.Archive(t.Context(), commit, path)
	if err != nil {
		t.Fatal(err)
	}

	files := map[string]string{}
	reader := tar.NewReader(archive)

	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}

		if nextErr != nil {
			t.Fatal(nextErr)
		}

		if header.Typeflag != tar.TypeReg {
			continue
		}

		content, readErr := io.ReadAll(reader)
		if readErr != nil {
			t.Fatal(readErr)
		}

		files[header.Name] = string(content)
	}

	err = archive.Close()
	if err != nil {
		t.Fatal(err)
	}

	return files
}

func TestArchiveIgnoresMonitoredExportAttributes(t *testing.T) {
	t.Parallel()

	repository := newRepository(t)

	// A mirror created by an earlier release must be upgraded when it is opened again.
	writeRepositoryFile(t, repository, filepath.Join(".git", "info", "attributes"), legacyAttributes)

	_, err := repository.Init(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	files := map[string]string{
		"var/www/app/.gitattributes": ".gitattributes export-ignore\ntests/ export-ignore\nversion.php export-subst\n",
		"var/www/app/tests/unit.php": "<?php // test\n",
		"var/www/app/version.php":    "<?php return '$Format:%H$';\n",
	}
	for name, content := range files {
		writeRepositoryFile(t, repository, name, content)
	}

	commit, err := repository.Commit(t.Context(), "monitored export attributes")
	if err != nil {
		t.Fatal(err)
	}

	archived := archivedFiles(t, repository, commit, "var/www/app")
	if !maps.Equal(archived, files) {
		t.Fatalf("archive applied monitored export attributes: got %q, want %q", archived, files)
	}
}
