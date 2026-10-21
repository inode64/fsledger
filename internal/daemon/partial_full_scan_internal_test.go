package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/integrity"
)

// keepChanging rewrites path whenever the catalog reads it, so every observation of it stays unstable.
func keepChanging(t *testing.T, runner *worker, path string) {
	t.Helper()

	mutations := 0

	runner.catalog.BindCopiedHashes(func(record integrity.Record, _ string) (string, int64, bool) {
		if string(record.Path) == path {
			mutations++

			err := os.WriteFile(path, make([]byte, mutations), 0o600)
			if err != nil {
				t.Error(err)
			}
		}

		return "", 0, false
	})
}

// retryUnstable runs the reconciliation that unstable paths schedule a minute after their scan.
func retryUnstable(t *testing.T, runner *worker) {
	t.Helper()

	runner.unstableRetry = time.Now().Add(-time.Second)
	runner.flush(t.Context(), time.Now())
}

func checkPartial(t *testing.T, runner *worker, full bool) {
	t.Helper()

	if runner.status.Scan.Result != "partial" || runner.status.Scan.Full != full {
		t.Fatalf("scan %+v, want a partial scan with full=%v", runner.status.Scan, full)
	}
}

func TestPartialFullScanIsNotRepeatedByUnstableRetry(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	runner.cfg.Watch.Reconcile.Enabled = false
	hot := writeSource(t, runner, "hot", "original")
	writeSource(t, runner, "stable", "original")

	err := runner.reconcile(t.Context(), "Initial snapshot")
	if err != nil {
		t.Fatal(err)
	}

	certified := runner.status.Catalog.LastFullScan

	writeSource(t, runner, "hot", "changed")
	keepChanging(t, runner, hot)

	runner.fullScanRequested, runner.dirty = true, true
	runner.flush(t.Context(), time.Now())
	checkPartial(t, runner, true)

	if runner.fullScanRequested || runner.status.Catalog.LastFullScan != certified {
		t.Fatal("partial full scan left its request pending or certified the catalog")
	}

	retryUnstable(t, runner)
	checkPartial(t, runner, false)
}

func TestPartialFirstInventoryRetriesWithoutFullHashing(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	runner.cfg.Watch.Reconcile.Enabled = false
	hot := writeSource(t, runner, "hot", "original")
	writeSource(t, runner, "stable", "original")
	keepChanging(t, runner, hot)

	err := runner.reconcile(t.Context(), "Initial snapshot")
	if err != nil {
		t.Fatal(err)
	}

	checkPartial(t, runner, true)
	retryUnstable(t, runner)
	checkPartial(t, runner, false)

	runner.catalog.BindCopiedHashes(runner.mirror.CopiedHash)
	retryUnstable(t, runner)

	// Only a complete full pass certifies the catalog; the ordinary retry merely completes the inventory.
	if len(runner.unstable) != 0 || runner.status.Scan.Full || runner.status.Scan.Initialized {
		t.Fatal("ordinary retry missed the settled path or certified a full scan", runner.status.Scan)
	}
}
