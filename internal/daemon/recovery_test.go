package daemon_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/daemon"
	"github.com/inode64/fsledger/internal/resource"
)

//nolint:funlen,gocognit // The full lifecycle keeps ordered filesystem/Git assertions together.
func TestPollingMultipleRepositoriesAndRestart(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sourceA := filepath.Join(root, "sourceA")

	sourceB := filepath.Join(root, "sourceB")

	extraSource := filepath.Join(root, "extra")
	for _, path := range []string{sourceA, sourceB, extraSource} {
		err := os.Mkdir(path, 0o700)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(filepath.Join(path, "file"), []byte("initial"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	cfg := pollingCatalogConfig(t, root, sourceA, extraSource, sourceB)

	start := func() func() {
		t.Helper()

		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan error, 1)
		go func() { done <- daemon.Run(ctx, cfg, slog.New(slog.DiscardHandler)) }()

		return func() {
			t.Helper()
			cancel()

			select {
			case runErr := <-done:
				if runErr != nil {
					t.Error(runErr)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("shutdown timeout")
			}
		}
	}
	read := func(repo, path, want string) bool {
		//nolint:gosec // This test uses only its private temporary directory and the current test process.
		data, readErr := os.ReadFile(filepath.Join(root, "repos", repo, strings.TrimPrefix(path, "/")))

		return readErr == nil && string(data) == want
	}
	stop := start()

	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()

	waitFor(t, func() bool {
		return read("sourceA", filepath.Join(extraSource, "file"), "initial") &&
			read("sourceA", filepath.Join(sourceA, "file"), "initial") &&
			read("sourceB", filepath.Join(sourceB, "file"), "initial")
	})

	writeFileErr := os.WriteFile(filepath.Join(sourceB, "file"), []byte("polled"), 0o600)
	if writeFileErr != nil {
		t.Fatal(writeFileErr)
	}

	waitFor(t, func() bool { return read("sourceB", filepath.Join(sourceB, "file"), "polled") })
	stop()

	stopped = true

	writeFileErr2 := os.WriteFile(filepath.Join(sourceA, "file"), []byte("offline change"), 0o600)
	if writeFileErr2 != nil {
		t.Fatal(writeFileErr2)
	}

	stop = start()
	stopped = false

	waitFor(t, func() bool { return read("sourceA", filepath.Join(sourceA, "file"), "offline change") })
	stop()

	stopped = true

	//nolint:gosec // This test uses only its private temporary directory and the current test process.
	output, err := exec.CommandContext(
		t.Context(), "git", "-C", filepath.Join(root, "repos", "sourceA"), "log", "-1", "--format=%B",
	).Output()
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(output), "Reason: startup") {
		t.Fatalf("offline change not recorded by startup: %s", output)
	}
}

func pollingCatalogConfig(t *testing.T, root, sourceA, extraSource, sourceB string) *config.Config {
	t.Helper()

	directory := filepath.Join(root, "repositories.d")

	err := os.Mkdir(directory, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	for name, text := range map[string]string{
		mainYAML: fmt.Sprintf(`storage:
  path: %s/repos
runtime: %s/run
paths:
  repositories: [sourceA.yaml, repositories.d]
  excludes: [private.yaml]
watch:
  backend: polling
  reconcile:
    enabled: false
    interval: 50ms
commit:
  debounce: 20ms
  max_delay: 100ms
`, root, root),
		"sourceA.yaml": fmt.Sprintf("paths: [%s, %s]\n", sourceA, extraSource),
		"private.yaml": "- '**/private'",
		"repositories.d/sourceB.yml": fmt.Sprintf(`paths: [%s]
exclude: [private]
`, sourceB),
	} {
		writeErr := os.WriteFile(filepath.Join(root, name), []byte(text), 0o600)
		if writeErr != nil {
			t.Fatal(writeErr)
		}
	}

	cfg, err := config.Load(filepath.Join(root, mainYAML))
	if err != nil {
		t.Fatal(err)
	}

	return cfg
}

func TestPollingCancellationDuringStartupIsGraceful(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := pollingCatalogConfig(
		t,
		root,
		filepath.Join(root, "a"),
		filepath.Join(root, "extra"),
		filepath.Join(root, "b"),
	)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := daemon.Run(ctx, cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal("shutdown during catalog initialization was reported as a failure", err)
	}
}

// A repository that cannot start must not stop the others, as a lock held by another process would.
func TestRepositoryStartupFailureLeavesOthersRunning(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := pollingCatalogConfig(
		t,
		root,
		filepath.Join(root, "a"),
		filepath.Join(root, "extra"),
		filepath.Join(root, "b"),
	)

	lock, err := resource.LockDirectory(cfg.RepositoryPath("sourceB"))
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(lock)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- daemon.Run(ctx, cfg, slog.New(slog.DiscardHandler)) }()

	// The other repository publishing its status proves it runs; the daemon must not have returned meanwhile.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		select {
		case err = <-done:
			t.Fatal("one repository failure stopped the daemon", err)
		default:
		}

		_, err = os.Stat(filepath.Join(cfg.Runtime, "sourceA.json"))
		if err == nil {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("the other repository is not running", err)
		}
	}

	cancel()

	err = <-done
	if err == nil || !strings.Contains(err.Error(), "repository sourceB") {
		t.Fatal("the failed repository was not reported", err)
	}
}

// A repository whose start failed, as with a lock a CLI command held, starts once the cause is gone.
func TestFailedRepositoryStartsWhenRetried(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := pollingCatalogConfig(
		t,
		root,
		filepath.Join(root, "a"),
		filepath.Join(root, "extra"),
		filepath.Join(root, "b"),
	)

	lock, err := resource.LockDirectory(cfg.RepositoryPath("sourceB"))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- daemon.Run(ctx, cfg, slog.New(slog.DiscardHandler)) }()

	// Both repositories start together: once sourceA runs, sourceB has met its lock.
	waitForFile(t, filepath.Join(cfg.Runtime, "sourceA.json"))
	resource.Close(lock)
	waitForFile(t, filepath.Join(cfg.Runtime, "sourceB.json"))

	cancel()

	err = <-done
	if err != nil {
		t.Fatal("a recovered repository was reported as failed", err)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()

	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		_, err := os.Stat(path)
		if err == nil {
			return
		}

		if time.Now().After(deadline) {
			t.Fatal("never appeared:", path, err)
		}
	}
}
