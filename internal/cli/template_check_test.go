package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/cli"
)

// A template that parses but cannot render an event routed to its notifier fails check, so every
// alert of that kind is not left retrying in the outbox.
func TestCheckRejectsTemplateThatCannotRender(t *testing.T) {
	t.Parallel()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for _, directory := range []string{"notifiers", "templates"} {
		err = os.Mkdir(filepath.Join(root, directory), 0o700)
		if err != nil {
			t.Fatal(err)
		}
	}

	for name, text := range map[string]string{
		fixtureMainYaml: "runtime: " + root + "/run\nstorage: {path: " + root + "/state}\n" +
			"paths: {repositories: [system.yaml], notifiers: [notifiers], templates: templates}\n",
		fixtureSystemYaml:    "paths: ['" + root + "/present']\nnotifications: {use: [ops], events: [error]}\n",
		"notifiers/ops.yaml": "type: slack\nurl: https://hooks.example.org/services/x\ntemplate: first\n",
		// Error events carry no paths; parsing alone accepts the index.
		"templates/first.yaml": "subject: 'fsledger {{.Host}}'\nbody: '{{index .Paths 0}}'\n",
	} {
		err = os.WriteFile(filepath.Join(root, name), []byte(text), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	var output, stderr bytes.Buffer

	err = cli.Run([]string{checkCommand, "-c", filepath.Join(root, fixtureMainYaml)}, &output, &stderr)
	if err == nil || !strings.Contains(err.Error(), "notifier ops: template first fails for error events") {
		t.Fatalf("template failing to render accepted: %v %q", err, output.String())
	}
}
