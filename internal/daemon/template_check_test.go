package daemon_test

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/daemon"
)

// Startup renders notification templates before preparing state: one that only parses would keep
// every alert to its notifier retrying in the outbox.
func TestRunRejectsTemplateThatCannotRender(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for _, directory := range []string{"notifiers", "templates"} {
		err := os.Mkdir(filepath.Join(root, directory), 0o700)
		if err != nil {
			t.Fatal(err)
		}
	}

	for name, text := range map[string]string{
		mainYAML: "storage: {path: " + root + "/repos}\nruntime: " + root + "/run\n" +
			"paths: {repositories: [system.yaml], notifiers: [notifiers], templates: templates}\n",
		"system.yaml":        "paths: [" + root + "/source]\nnotifications: {use: [ops], events: [change]}\n",
		"notifiers/ops.yaml": "type: slack\nurl: https://hooks.example.org/services/x\ntemplate: first\n",
		// Only report messages carry a report; parsing alone accepts the field.
		"templates/first.yaml": "subject: 'fsledger {{.Host}}'\nbody: '{{.Report.ID}}'\n",
	} {
		err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := config.Load(filepath.Join(root, mainYAML))
	if err != nil {
		t.Fatal(err)
	}

	// Cancelled, so a startup that skipped the check returns instead of running the daemon.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err = daemon.Run(ctx, cfg, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "notifier ops: template first fails for change events") {
		t.Fatal("template failing to render accepted at startup:", err)
	}

	_, err = os.Stat(filepath.Join(root, "repos"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("state prepared before templates were checked:", err)
	}
}
