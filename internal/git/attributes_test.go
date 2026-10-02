package git_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestLiteralContent(t *testing.T) {
	t.Parallel()

	repository := newRepository(t)

	attributes := "* text ident\nencoded working-tree-encoding=UTF-16LE\nfiltered filter=reject\n"

	writeErr := os.WriteFile(filepath.Join(repository.Path, ".gitattributes"), []byte(attributes), 0o600)
	if writeErr != nil {
		t.Fatal(writeErr)
	}

	for key, value := range map[string]string{"filter.reject.clean": "false", "filter.reject.required": "true"} {
		//nolint:gosec // The deliberately failing filter uses literal local test configuration.
		_, configErr := exec.CommandContext(t.Context(), "git", "-C", repository.Path, "config", key, value).Output()
		if configErr != nil {
			t.Fatal(configErr)
		}
	}

	files := map[string]string{
		"crlf":       "line one\r\nline two\r\n",
		"identifier": "$Id: original bytes $\n",
		"encoded":    "a\x00b\x00\n\x00",
		"filtered":   "retain these bytes\n",
	}
	for name, content := range files {
		writeErr = os.WriteFile(filepath.Join(repository.Path, name), []byte(content), 0o600)
		if writeErr != nil {
			t.Fatal(writeErr)
		}
	}

	_, commitErr := repository.Commit(t.Context(), "preserve literal source content")
	if commitErr != nil {
		t.Fatal(commitErr)
	}

	for name, content := range files {
		output := repositoryOutput(t, repository, "show", "HEAD:"+name)
		if output != content {
			t.Errorf("Git transformed %s: got %q, want %q", name, output, content)
		}
	}

	// Incremental staging must preserve the same byte-level contract.
	for name, content := range files {
		writeRepositoryFile(t, repository, name, content+content)
		commitPaths(t, repository, name)

		if got := repositoryOutput(t, repository, "show", "HEAD:"+name); got != content+content {
			t.Errorf("incremental staging transformed %s", name)
		}
	}
}
