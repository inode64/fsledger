package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A repository waiting for a recorded filesystem is in an expected state: its stop is no daemon failure,
// which would make the process exit non-zero and leave a systemd unit failed on every stop.
func TestShutdownWhileWaitingForAFilesystemIsNoFailure(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	nested := filepath.Join(runner.roots[0], "data")

	err := os.Mkdir(nested, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = runner.reconcile(t.Context(), "first")
	if err != nil {
		t.Fatal(err)
	}

	// A plain directory recorded as a mount point stands for an unmounted filesystem.
	runner.mounts = []string{nested}

	operation, cancel := context.WithCancel(t.Context())
	cancel()

	err = runner.shutdown(operation)
	if err != nil || runner.status.Running {
		t.Fatal("waiting for a filesystem failed the stop", err)
	}
}
