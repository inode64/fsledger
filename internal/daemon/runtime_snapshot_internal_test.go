package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
)

// Snapshots of repositories no longer configured go at startup, and a repository that fails after taking
// its lock leaves no snapshot of the previous process to read as its state.
func TestRuntimeKeepsOnlySnapshotsOfThisProcess(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "source")
	runtime := filepath.Join(root, "run")

	for _, path := range []string{source, runtime} {
		err := os.Mkdir(path, 0o700)
		if err != nil {
			t.Fatal(err)
		}
	}

	for name, text := range map[string]string{
		"main.yaml": fmt.Sprintf(`storage: {path: %s/repos}
runtime: %s
paths: {repositories: [system.yaml]}
watch: {backend: polling, reconcile: {interval: 1h, on_start: false, on_stop: false}}`, root, runtime),
		"system.yaml": fmt.Sprintf("paths: [%s]", source),
	} {
		err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := config.Load(filepath.Join(root, "main.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	old := []byte(`{"repository":"x","running":true}`)
	for _, name := range []string{"removed.json", "removed.flush", "system.json"} {
		err = os.WriteFile(filepath.Join(runtime, name), old, 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	// A catalog that cannot open fails the repository after its lock.
	err = errors.Join(os.MkdirAll(filepath.Dir(cfg.CatalogPath("system")), 0o700),
		os.WriteFile(cfg.CatalogPath("system"), nil, 0o600))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	runErr := Run(ctx, cfg, slog.New(slog.DiscardHandler))
	if runErr == nil {
		t.Fatal("the repository started with a broken catalog")
	}

	for _, name := range []string{"removed.json", "removed.flush", "system.json"} {
		_, statErr := os.Lstat(filepath.Join(runtime, name))
		if !os.IsNotExist(statErr) {
			t.Fatal("stale runtime file kept:", name, statErr)
		}
	}
}
