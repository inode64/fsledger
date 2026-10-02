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
)

//nolint:cyclop,funlen,gocognit,gocyclo // The full lifecycle keeps ordered filesystem/Git assertions together.
func TestInotifyEndToEnd(t *testing.T) {
	t.Parallel()

	_, lookPathErr := exec.LookPath("git")
	if lookPathErr != nil {
		t.Skip("git unavailable")
	}

	root := t.TempDir()
	source := filepath.Join(root, "source")
	storage := filepath.Join(root, "repos")
	runtime := filepath.Join(root, "run")

	mkdirErr := os.Mkdir(source, 0o700)
	if mkdirErr != nil {
		t.Fatal(mkdirErr)
	}

	write := func(name, value string) {
		t.Helper()

		err := os.WriteFile(filepath.Join(source, name), []byte(value), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}
	write("a", "initial")

	for name, text := range map[string]string{
		mainYAML: fmt.Sprintf(`storage: {path: %s}
runtime: %s
paths: {repositories: [system.yaml], excludes: [common.yaml]}
watch: {backend: inotify, reconcile: {interval: 100ms}}
commit: {debounce: 40ms, max_delay: 200ms}
exclude: [common]
`, storage, runtime),
		systemYAML:    fmt.Sprintf("paths: [%s]", source),
		"common.yaml": "- '**/*.secret'",
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

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, cfg, slog.New(slog.DiscardHandler)) }()

	defer func() {
		cancel()

		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon shutdown timed out")
		}
	}()

	mirrorPath := filepath.Join(storage, "system", strings.TrimPrefix(source, "/"))

	waitFor(t, func() bool {
		//nolint:gosec // This test uses only its private temporary directory and the current test process.
		data, e := os.ReadFile(filepath.Join(mirrorPath, "a"))

		return e == nil && string(data) == "initial"
	})
	write("a", "changed")
	waitFor(t, func() bool {
		//nolint:gosec // This test uses only its private temporary directory and the current test process.
		data, e := os.ReadFile(filepath.Join(mirrorPath, "a"))

		return e == nil && string(data) == "changed"
	})
	write("hidden.secret", "do not copy")

	renameErr := os.Rename(filepath.Join(source, "a"), filepath.Join(source, "b"))
	if renameErr != nil {
		t.Fatal(renameErr)
	}

	waitFor(t, func() bool {
		//nolint:gosec // This test uses only its private temporary directory and the current test process.
		data, e := os.ReadFile(filepath.Join(mirrorPath, "b"))
		_, old := os.Lstat(filepath.Join(mirrorPath, "a"))

		return e == nil && string(data) == "changed" && os.IsNotExist(old)
	})

	removeErr := os.Remove(filepath.Join(source, "b"))
	if removeErr != nil {
		t.Fatal(removeErr)
	}

	waitFor(t, func() bool {
		_, e := os.Lstat(filepath.Join(mirrorPath, "b"))

		return os.IsNotExist(e)
	})

	_, lstatErr := os.Lstat(filepath.Join(mirrorPath, "hidden.secret"))
	if !os.IsNotExist(lstatErr) {
		t.Fatalf("excluded secret copied: %v", lstatErr)
	}

	_, lstatErr2 := os.Lstat(filepath.Join(source, ".git"))
	if !os.IsNotExist(lstatErr2) {
		t.Fatalf("source acquired Git metadata: %v", lstatErr2)
	}

	waitFor(t, func() bool {
		//nolint:gosec // This test uses only its private temporary directory and the current test process.
		out, e := exec.CommandContext(t.Context(), "git", "-C", filepath.Join(storage, "system"), "log", "--format=%s").
			Output()

		return e == nil && strings.Contains(string(out), "Initial snapshot") && strings.Count(string(out), "\n") >= 3
	})
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()

	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		if condition() {
			return
		}

		select {
		case <-deadline.C:
			t.Fatal("condition timed out")
		case <-ticker.C:
		}
	}
}
