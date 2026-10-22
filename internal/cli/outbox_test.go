package cli_test

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/cli"
)

func writeOutboxFixture(t *testing.T, root string) {
	t.Helper()

	writeFiles(t, root, map[string]string{
		"source":     "contents",
		"mail.yaml":  fixtureMailNotifier,
		"repo.yaml":  "type: db\npaths: ['" + root + "/source']\n",
		"other.yaml": "type: db\npaths: ['" + root + "/other']\n",
		fixtureMainYaml: "runtime: '" + root + "/run'\nstorage: {path: '" + root + "/state'}\n" +
			"paths: {repositories: [repo.yaml, other.yaml], notifiers: [mail.yaml]}\n" +
			"notifications: {use: [mail]}\n",
	})
}

func TestOutboxClearDiscardsQueuedNotificationsOnce(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeOutboxFixture(t, root)

	var output, stderr bytes.Buffer

	configuration := []string{"-c", filepath.Join(root, fixtureMainYaml)}
	invoke := func(args ...string) error {
		output.Reset()

		return cli.Run(append(args, configuration...), &output, &stderr)
	}

	err := invoke(verifyCommand, "--repository", "repo")
	if !errors.Is(err, cli.ErrChanges) {
		t.Fatal(output.String(), err)
	}

	err = invoke("outbox", "clear", "--repository", "repo")
	if err != nil || !strings.HasPrefix(output.String(), "Discarded ") ||
		strings.HasPrefix(output.String(), "Discarded 0 ") {
		t.Fatal("first clear discarded nothing", output.String(), err)
	}

	err = invoke("outbox", "clear", "--repository", "repo")
	if err != nil || output.String() != "Discarded 0 pending notifications from repo\n" {
		t.Fatal("second clear found a queue", output.String(), err)
	}

	err = invoke("outbox", "list", "--repository", "repo")
	if err == nil {
		t.Fatal("unknown outbox action accepted")
	}

	err = invoke("outbox", "clear", "--repository", "other")
	if err == nil {
		t.Fatal("repository without catalog accepted")
	}

	matches, err := filepath.Glob(filepath.Join(root, "state", "other", "*"))
	if err != nil || len(matches) != 0 {
		t.Fatal("clear created catalog storage", matches)
	}
}
