package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/daemon"
)

const mainYAML = "main.yaml"

type sourcesFixture struct {
	root, tree, storage, runtime string
}

const (
	systemYAML = "system.yaml"
	systemName = "system"
)

func newSourcesFixture(t *testing.T) sourcesFixture {
	t.Helper()

	_, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	fixture := sourcesFixture{
		root: root, tree: filepath.Join(root, "tree"),
		storage: filepath.Join(root, "repos"), runtime: filepath.Join(root, "run"),
	}

	err = os.Mkdir(fixture.tree, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	return fixture
}

// startSources runs a daemon over one Git repository named system whose paths live below tree.
func startSources(t *testing.T, entries ...string) sourcesFixture {
	t.Helper()

	fixture := newSourcesFixture(t)

	paths := make([]string, len(entries))
	for index, entry := range entries {
		paths[index] = "'" + filepath.Join(fixture.tree, entry) + "'"
	}

	fixture.run(t, map[string]string{systemYAML: "paths: [" + strings.Join(paths, ", ") + "]"})

	return fixture
}

// run starts the daemon over the given repository files and stops it with the test.
func (fixture sourcesFixture) run(t *testing.T, repositories map[string]string) {
	t.Helper()

	names := make([]string, 0, len(repositories))
	for name, text := range repositories {
		names = append(names, name)

		err := os.WriteFile(filepath.Join(fixture.root, name), []byte(text), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	slices.Sort(names)

	main := fmt.Sprintf(`storage: {path: %s}
runtime: %s
paths: {repositories: [%s]}
watch: {backend: inotify, reconcile: {interval: 100ms}}
commit: {debounce: 40ms, max_delay: 200ms}
integrity: {reference: previous}
`, fixture.storage, fixture.runtime, strings.Join(names, ", "))

	err := os.WriteFile(filepath.Join(fixture.root, mainYAML), []byte(main), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(filepath.Join(fixture.root, mainYAML))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, cfg, slog.New(slog.DiscardHandler)) }()

	t.Cleanup(func() {
		cancel()

		select {
		case runErr := <-done:
			if runErr != nil {
				t.Error(runErr)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon shutdown timed out")
		}
	})
}

func (fixture sourcesFixture) write(t *testing.T, name, value string) {
	t.Helper()

	path := filepath.Join(fixture.tree, name)

	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		err = os.WriteFile(path, []byte(value), 0o600)
	}

	if err != nil {
		t.Fatal(err)
	}
}

func (fixture sourcesFixture) mirrored(name, want string) bool {
	//nolint:gosec // This test uses only its private temporary directory.
	data, err := os.ReadFile(filepath.Join(fixture.storage, systemName, strings.TrimPrefix(fixture.tree, "/"), name))

	return err == nil && string(data) == want
}

func (fixture sourcesFixture) status(t *testing.T, repository string) daemon.Status {
	t.Helper()

	var status daemon.Status
	//nolint:gosec // This test uses only its private temporary directory.
	data, err := os.ReadFile(filepath.Join(fixture.runtime, repository+".json"))
	if err == nil {
		err = json.Unmarshal(data, &status)
	}

	if err != nil {
		return daemon.Status{}
	}

	return status
}

func TestAbsentSourcesDoNotStopDaemon(t *testing.T) {
	t.Parallel()

	fixture := startSources(t, "absent/config.php", "keys/backup*.keyfile")

	waitFor(t, func() bool {
		status := fixture.status(t, systemName)

		return status.Running && len(status.Paths) == 0 && !status.LastReconciliation.IsZero() &&
			slices.Equal(status.MissingSources, []string{
				filepath.Join(
					fixture.tree,
					"absent",
					"config.php",
				),
				filepath.Join(fixture.tree, "keys", "backup*.keyfile"),
			})
	})

	if warnings := fixture.status(t, systemName).Warnings; len(warnings) != 0 {
		t.Fatal("absent sources reported as a problem:", warnings)
	}
}

func TestSourcesAppearingLaterAreAdopted(t *testing.T) {
	t.Parallel()

	fixture := startSources(t, "present", "late/config.php", "keys/backup*.keyfile")
	fixture.write(t, "present/file", "initial")
	waitFor(t, func() bool { return fixture.mirrored("present/file", "initial") })

	fixture.write(t, "late/config.php", "late")
	fixture.write(t, "keys/backup1.keyfile", "key")
	fixture.write(t, "keys/unrelated", "no")
	waitFor(t, func() bool {
		return fixture.mirrored("late/config.php", "late") && fixture.mirrored("keys/backup1.keyfile", "key")
	})

	// Adopted sources are watched like the initial ones, and unselected siblings stay out.
	fixture.write(t, "late/config.php", "changed")
	waitFor(t, func() bool { return fixture.mirrored("late/config.php", "changed") })

	status := fixture.status(t, systemName)
	if fixture.mirrored("keys/unrelated", "no") || len(status.MissingSources) != 0 || len(status.Paths) != 3 {
		t.Fatalf("unexpected sources: %+v", status)
	}
}

func TestDeletedDirectorySourceIsRecordedWithoutError(t *testing.T) {
	t.Parallel()

	fixture := startSources(t, "kept", "mods/*/etc")
	fixture.write(t, "kept/file", "kept")
	fixture.write(t, "mods/a/etc/module.conf", "module")
	waitFor(t, func() bool { return fixture.mirrored("mods/a/etc/module.conf", "module") })

	err := os.RemoveAll(filepath.Join(fixture.tree, "mods", "a"))
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool {
		_, statErr := os.Lstat(
			filepath.Join(fixture.storage, systemName, strings.TrimPrefix(fixture.tree, "/"), "mods", "a"),
		)

		return os.IsNotExist(statErr) && len(fixture.status(t, systemName).Paths) == 1
	})

	status := fixture.status(t, systemName)
	if len(status.Warnings) != 0 || !fixture.mirrored("kept/file", "kept") {
		t.Fatalf("deleting a selected directory was treated as a failure: %+v", status)
	}
}

func TestDeletingTheOnlySourceIsRecordedWithoutError(t *testing.T) {
	t.Parallel()

	fixture := startSources(t, "mods/*/etc")
	fixture.write(t, "mods/a/etc/module.conf", "module")
	waitFor(t, func() bool { return fixture.mirrored("mods/a/etc/module.conf", "module") })

	err := os.RemoveAll(filepath.Join(fixture.tree, "mods", "a"))
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool {
		_, statErr := os.Lstat(
			filepath.Join(fixture.storage, systemName, strings.TrimPrefix(fixture.tree, "/"), "mods", "a"),
		)

		return os.IsNotExist(statErr) && len(fixture.status(t, systemName).Paths) == 0
	})

	status := fixture.status(t, systemName)
	if len(status.Warnings) != 0 {
		t.Fatalf("deleting the only selected directory was treated as a failure: %+v", status)
	}
}

func TestGitCopyAndDBInventoryShareSource(t *testing.T) {
	t.Parallel()

	fixture := newSourcesFixture(t)
	fixture.write(t, "site/htdocs/wp-config.php", "secret-one")
	fixture.write(t, "site/htdocs/index.php", "page")
	fixture.run(t, map[string]string{
		systemYAML:  "type: git\npaths: ['" + fixture.tree + "/*/htdocs/wp-config.php']",
		"site.yaml": "type: db\npaths: ['" + fixture.tree + "/site/htdocs']",
	})

	waitFor(t, func() bool {
		return fixture.mirrored("site/htdocs/wp-config.php", "secret-one") &&
			fixture.status(t, "site").Catalog.Entries == 3
	})

	before := fixture.status(t, "site").LastChange

	fixture.write(t, "site/htdocs/wp-config.php", "secret-two")
	waitFor(t, func() bool {
		return fixture.mirrored("site/htdocs/wp-config.php", "secret-two") &&
			fixture.status(t, "site").LastChange != before
	})

	if fixture.mirrored("site/htdocs/index.php", "page") {
		t.Fatal("the Git copy took files selected only by the DB inventory")
	}
}
