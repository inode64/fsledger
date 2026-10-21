package daemon_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/daemon"
	"github.com/inode64/fsledger/internal/integrity"
)

// configure writes one Git repository named system selecting entries below tree, with run's settings.
func (fixture sourcesFixture) configure(t *testing.T, entries ...string) *config.Config {
	t.Helper()

	paths := make([]string, len(entries))
	for index, entry := range entries {
		paths[index] = "'" + filepath.Join(fixture.tree, entry) + "'"
	}

	main := fmt.Sprintf(`storage: {path: %s}
runtime: %s
paths: {repositories: [%s]}
watch: {backend: inotify, reconcile: {interval: 100ms}}
commit: {debounce: 40ms, max_delay: 200ms}
integrity: {reference: previous}
`, fixture.storage, fixture.runtime, systemYAML)

	for name, text := range map[string]string{mainYAML: main, systemYAML: "paths: [" + strings.Join(paths, ", ") + "]"} {
		err := os.WriteFile(filepath.Join(fixture.root, name), []byte(text), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := config.Load(filepath.Join(fixture.root, mainYAML))
	if err != nil {
		t.Fatal(err)
	}

	return cfg
}

// start runs the daemon like run, but returns its stop so one test can restart it. Stop returns the
// status saved at shutdown and removes it, so the next start is never judged by that snapshot. A run
// the test leaves to cleanup may end with the error of a shutdown reconciliation it made fail.
func (fixture sourcesFixture) start(t *testing.T, entries ...string) func() (daemon.Status, error) {
	t.Helper()

	cfg := fixture.configure(t, entries...)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, cfg, slog.New(slog.DiscardHandler)) }()

	stop := sync.OnceValues(func() (daemon.Status, error) {
		cancel()

		var runErr error

		select {
		case runErr = <-done:
		case <-time.After(10 * time.Second):
			t.Error("daemon shutdown timed out")
		}

		status := fixture.status(t, systemName)

		removeErr := os.Remove(filepath.Join(fixture.runtime, systemName+".json"))
		if removeErr != nil && !os.IsNotExist(removeErr) {
			t.Error(removeErr)
		}

		return status, runErr
	})

	t.Cleanup(func() {
		_, stopErr := stop()
		if stopErr != nil {
			t.Log("daemon stopped with:", stopErr)
		}
	})

	return stop
}

// restarted waits for the startup reconciliation to complete, or to refuse an unavailable source.
func (fixture sourcesFixture) restarted(t *testing.T) daemon.Status {
	t.Helper()

	var status daemon.Status

	waitFor(t, func() bool {
		status = fixture.status(t, systemName)

		return status.Running && (!status.LastReconciliation.IsZero() ||
			slices.ContainsFunc(status.Warnings, func(warning string) bool {
				return strings.Contains(warning, integrity.ErrUnavailable.Error())
			}))
	})

	return status
}

// pointLink replaces a symlink, so a test can make a source unreadable even for root.
func pointLink(t *testing.T, link, target string) {
	t.Helper()

	err := os.Remove(link)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	err = os.Symlink(target, link)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRestartKeepsUnreadableLiteralSource(t *testing.T) {
	t.Parallel()

	fixture := newSourcesFixture(t)
	fixture.write(t, "kept/file", "kept")
	fixture.write(t, "real/data/file", "literal")

	link := filepath.Join(fixture.tree, "link")
	pointLink(t, link, "real")

	stop := fixture.start(t, "kept", "link/data")
	waitFor(t, func() bool {
		return fixture.mirrored("kept/file", "kept") && fixture.mirrored("real/data/file", "literal")
	})

	before, err := stop()
	if err != nil {
		t.Fatal(err)
	}

	// A symlink loop in the parent exceeds the hop limit: the entry is unread, not deleted.
	pointLink(t, link, "link")
	fixture.start(t, "kept", "link/data")

	status := fixture.restarted(t)
	if before.Catalog.Entries == 0 || status.Catalog.Entries != before.Catalog.Entries ||
		!fixture.mirrored("real/data/file", "literal") {
		t.Fatalf("restart pruned an unreadable source: before=%d after=%d paths=%v",
			before.Catalog.Entries, status.Catalog.Entries, status.Paths)
	}
}

func TestRestartKeepsUnreadableGlobMatches(t *testing.T) {
	t.Parallel()

	fixture := newSourcesFixture(t)
	fixture.write(t, "kept/file", "kept")
	fixture.write(t, "mods/a/file", "glob")

	stop := fixture.start(t, "kept", "mods/*")
	waitFor(t, func() bool { return fixture.mirrored("kept/file", "kept") && fixture.mirrored("mods/a/file", "glob") })

	before, err := stop()
	if err != nil {
		t.Fatal(err)
	}

	mods := filepath.Join(fixture.tree, "mods")

	err = os.Rename(mods, mods+".saved")
	if err != nil {
		t.Fatal(err)
	}

	pointLink(t, mods, "mods")
	fixture.start(t, "kept", "mods/*")

	// The pattern cannot be expanded, so its recorded match is kept and blocks pruning.
	status := fixture.restarted(t)
	if before.Catalog.Entries == 0 || status.Catalog.Entries != before.Catalog.Entries ||
		!slices.Contains(status.Paths, filepath.Join(mods, "a")) || !fixture.mirrored("mods/a/file", "glob") {
		t.Fatalf("restart pruned the matches of an unreadable pattern: before=%d after=%d paths=%v",
			before.Catalog.Entries, status.Catalog.Entries, status.Paths)
	}
}
