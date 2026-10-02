package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/event"
)

func TestScanClockSkipsMissedSlotsAndLeavesIntervalAfterCompletion(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, time.September, 21, 0, 0, 0, 0, time.UTC)

	clock, err := newScanClock(start, 0, "* * * * *", "UTC")
	if err != nil {
		t.Fatal(err)
	}

	if clock.due(start) || !clock.due(start.Add(time.Minute)) || clock.due(start.Add(time.Minute)) {
		t.Fatal("incorrect cron admission")
	}

	clock.advance(start.Add(5*time.Minute), false)

	if clock.skipped != 4 || !clock.next.Equal(start.Add(6*time.Minute)) {
		t.Fatal("queued missed cron slots", clock)
	}

	interval, err := newScanClock(start, time.Minute, "", "UTC")
	if err != nil {
		t.Fatal(err)
	}

	interval.advance(start.Add(5*time.Minute), false)

	if interval.due(start.Add(5*time.Minute)) || !interval.next.Equal(start.Add(6*time.Minute)) {
		t.Fatal("interval polling became continuous")
	}
}

func TestHashCronDoesNotRehashAtEveryOrdinaryScan(t *testing.T) {
	t.Parallel()

	now := time.Now()
	policy := config.Hash{}

	policy.FullScanSchedule = "15 3 * * *"
	if fullScanDue(policy, now.Add(-48*time.Hour).UnixNano(), now) {
		t.Fatal("cron hash policy became an always-due zero interval")
	}

	if !fullScanDue(policy, 0, now) {
		t.Fatal("first inventory omitted full hashing")
	}

	policy.FullScanSchedule, policy.FullScanInterval = "", time.Hour
	if !fullScanDue(policy, now.Add(-2*time.Hour).UnixNano(), now) {
		t.Fatal("interval hash policy stopped checking age")
	}
}

func TestPollingRunsExistingWholeRepositoryScanner(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	runner.repo, runner.mirror = nil, nil
	runner.catalog.BindCopiedHashes(nil)
	runner.cfg.Watch.Backend = event.Polling
	runner.cfg.Watch.Reconcile.Schedule = "* * * * *"
	runner.cfg.Watch.Reconcile.Interval = 0
	runner.cfg.Integrity.Hash.FullScanSchedule = "* * * * *"
	runner.cfg.Integrity.Hash.FullScanInterval = 0
	start := time.Now().UTC().Truncate(time.Minute)

	err := runner.prepareScanSchedule(start)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(runner.roots[0], "created")

	err = os.WriteFile(path, []byte("first version"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	runner.startWatchers(t.Context())

	if len(runner.watchers) != 0 || runner.status.FanotifyPIDFDLifetime != nil {
		t.Fatal("polling created event watchers")
	}

	runner.checkScanSchedule(start.Add(time.Minute))

	if !runner.dirty || !runner.fullScanRequested {
		t.Fatal("coincident hash and metadata scan not combined")
	}

	runner.flushReconciliation(t.Context(), start.Add(time.Minute))

	if runner.dirty || !runner.status.Scan.Full || runner.status.Scan.Observed == 0 || runner.status.Scan.Hashed != 1 {
		t.Fatalf("existing scanner not reused: %+v", runner.status.Scan)
	}

	err = os.Remove(path)
	if err != nil {
		t.Fatal(err)
	}

	runner.checkScanSchedule(runner.scans.ordinary.next)
	runner.flushReconciliation(t.Context(), time.Now())

	if runner.status.Scan.Result != "complete" || runner.dirty {
		t.Fatal("whole repository deletion scan failed", runner.status.Scan)
	}
}

func TestCronPollingFailureWaitsForNextSlot(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	runner.cfg.Watch.Backend = event.Polling
	runner.cfg.Watch.Reconcile.Schedule = "0 3 * * *"
	runner.failedReconciliation(os.ErrPermission)

	if runner.dirty || runner.fullScanRequested || !runner.pendingError {
		t.Fatal("failed cron scan would retry continuously")
	}
}

func TestPollingDeferredStartupAndShutdownDoNotScan(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	runner.cfg.Watch.Backend = event.Polling
	runner.cfg.Watch.Reconcile.OnStart, runner.cfg.Watch.Reconcile.OnStop = false, false
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := runner.run(ctx, true)
	if err != nil {
		t.Fatal(err)
	}

	if !runner.status.Scan.Started.IsZero() || runner.status.Scan.Initialized {
		t.Fatal("deferred polling scanned on startup or shutdown", runner.status.Scan)
	}
}
