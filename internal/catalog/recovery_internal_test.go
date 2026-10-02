package catalog

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/resource"
)

func TestCrashHelper(t *testing.T) {
	t.Parallel()

	root := os.Getenv("FSLEDGER_CRASH_ROOT")
	if root == "" {
		t.Skip("subprocess fixture")
	}

	cfg, err := config.Load(testConfiguration(t))
	if err != nil {
		t.Fatal(err)
	}

	cfg.Integrity.Compare = []string{fixtureType, fixtureMode}
	cfg.Notifications.Use = []string{"test"}
	cfg.Notifications.BatchWindow = 0

	store, err := Open(
		t.Context(),
		filepath.Join(root, config.CatalogDirectory),
		"test",
		"host",
		cfg.Integrity,
		cfg.Notifications,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(store)

	original := integrity.Record{Path: []byte("/file"), Type: fixtureRegular, Mode: 0o600}
	reference(t, store, original)
	changed := original
	changed.Mode = 0o700
	observeRecords(t, store, "changed", changed)

	err = store.SaveDeferred(
		t.Context(),
		map[string]Deferred{"/pending": {Signature: "staged", Policy: "policy", Since: 123}},
	)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate termination after a durable shadow reference copy, before pointer publication.
	err = store.copyBaseline(t.Context(), 1-store.state.Baseline)
	if err != nil {
		t.Fatal(err)
	}

	//nolint:gosec // Parent-provided private temporary fixture directory.
	err = os.WriteFile(filepath.Join(root, "ready"), []byte("durable"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(time.Minute)
	t.Fatal("parent did not terminate fixture")
}

//nolint:cyclop,funlen,gocognit,gocyclo // Ordered subprocess lifecycle verifies durable state across an actual SIGKILL.
func TestSIGKILLRecoversReferenceAndPendingDelivery(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	//nolint:gosec // Runs only this compiled test executable with a fixed helper selector.
	child := exec.CommandContext(t.Context(), executable, "-test.run=^TestCrashHelper$")

	child.Env = append(os.Environ(), "FSLEDGER_CRASH_ROOT="+root)

	err = child.Start()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		killErr := child.Process.Kill()
		if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			t.Log(killErr)
		}
	})

	deadline := time.Now().Add(20 * time.Second)

	for {
		_, err = os.Stat(filepath.Join(root, "ready"))
		if err == nil {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("child did not acknowledge durable writes")
		}

		time.Sleep(10 * time.Millisecond)
	}

	err = child.Process.Kill()
	if err != nil {
		t.Fatal(err)
	}

	err = child.Wait()
	if err == nil {
		t.Fatal("fixture was not killed")
	}

	cfg, err := config.Load(testConfiguration(t))
	if err != nil {
		t.Fatal(err)
	}

	cfg.Integrity.Compare = []string{fixtureType, fixtureMode}

	store, err := Open(
		t.Context(),
		filepath.Join(root, config.CatalogDirectory),
		"test",
		"host",
		cfg.Integrity,
		cfg.Notifications,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(store)

	deferred, err := store.Deferred(t.Context())
	if err != nil || deferred["/pending"].Signature != "staged" {
		t.Fatal("acknowledged deferred path lost", err)
	}

	original, _, err := readRecord(store.database, baselinePrefix(store.state.Baseline), []byte("/file"))
	if err != nil || original.Mode != 0o600 {
		t.Fatal("unpublished reference became approved", err)
	}

	current, exists, err := store.Current(t.Context(), []byte("/file"))
	if err != nil || !exists || current.Mode != 0o700 {
		t.Fatal("acknowledged observation lost", err)
	}

	due, err := store.Due(t.Context())
	if err != nil || len(due) == 0 {
		t.Fatal("acknowledged notification lost", err)
	}

	err = store.Accept(t.Context(), "changed")
	if err != nil {
		t.Fatal(err)
	}
}
