package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/cli"
)

func writeOutboxFixture(t *testing.T, root string) {
	t.Helper()

	for name, body := range map[string]string{
		"source": "contents",
		"mail.yaml": `type: email
dsn: smtps://unreachable.invalid
from: sender@example.org
to: [reader@example.org]
`,
		"repo.yaml":  "type: db\npaths: ['" + root + "/source']\n",
		"other.yaml": "type: db\npaths: ['" + root + "/other']\n",
		fixtureMainYaml: "runtime: '" + root + "/run'\nstorage: {path: '" + root + "/state'}\n" +
			"paths: {repositories: [repo.yaml, other.yaml], notifiers: [mail.yaml]}\n" +
			"integrity: {reference: previous}\nnotifications: {use: [mail]}\n",
	} {
		err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}
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
	if err != nil {
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
