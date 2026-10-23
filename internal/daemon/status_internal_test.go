package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/watcher"
)

func TestStatusCountsWorkerAndDetectorQueues(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	raw := event.Raw{Path: writeSource(t, runner, "file", "pending"), Time: time.Now()}
	runner.accept(raw)
	runner.queue.Send(raw)
	runner.queue.Send(raw)

	detector := watcher.NewQueue(1)
	detector.Send(raw)
	runner.watchers = []watcher.Watcher{&queuedDetector{Queue: detector}}
	runner.saveStatus(t.Context())

	if runner.status.PendingGroups != 1 || runner.status.PendingEvents != 4 {
		t.Fatalf("status omits pending work: groups=%d events=%d",
			runner.status.PendingGroups, runner.status.PendingEvents)
	}
}

func TestGitStatusTimeoutDoesNotDiscardCatalogStatus(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	writeSource(t, runner, "file", "observed")

	err := runner.reconcile(t.Context(), "initial")
	if err != nil {
		t.Fatal(err)
	}

	want, err := runner.catalog.Stats(t.Context())
	if err != nil || want.Entries == 0 {
		t.Fatal("missing inventory", want, err)
	}

	index := filepath.Join(runner.repo.Path, ".git", "index")

	err = os.Remove(index)
	if err != nil {
		t.Fatal(err)
	}

	// A FIFO makes the real Git command wait until its status deadline cancels it.
	err = unix.Mkfifo(index, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	runner.gitStatusNext = time.Time{}
	runner.status.Catalog = catalog.Stats{}
	runner.saveStatus(t.Context())

	if !strings.Contains(runner.status.GitStatus, "deadline exceeded") {
		t.Fatal("Git did not reach the status deadline", runner.status.GitStatus)
	}

	if runner.status.Catalog != want {
		t.Fatal("Git timeout discarded independent catalog counters", runner.status.Catalog, want)
	}
}

// Notifications the outbox stopped retrying must stay visible in the status, once, until outbox clear.
func TestStatusWarnsAboutExhaustedNotifications(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	runner.catalog.Notifications.Use = []string{opsNotifier}
	runner.catalog.Notifications.BatchWindow = 0

	err := runner.catalog.Announce(t.Context(), config.NotificationError, "")
	if err != nil {
		t.Fatal(err)
	}

	due, err := runner.catalog.Due(t.Context())
	if err != nil || len(due) != 1 {
		t.Fatalf("due=%d error=%v", len(due), err)
	}

	for range catalog.MaxDeliveryAttempts {
		_, err = runner.catalog.RetryMany(t.Context(), []int64{due[0].ID}, time.Second, "SMTP authentication failed")
		if err != nil {
			t.Fatal(err)
		}
	}

	exhaustedWarnings := func() int {
		count := 0

		for _, warning := range runner.status.Warnings {
			if strings.Contains(warning, "1 queued notifications stopped retrying") {
				count++
			}
		}

		return count
	}

	runner.saveStatus(t.Context())
	runner.saveStatus(t.Context())

	if exhaustedWarnings() != 1 || runner.status.Catalog.ExhaustedNotifications != 1 {
		t.Fatalf("exhausted notifications not reported once: %v", runner.status.Warnings)
	}

	_, err = runner.catalog.DiscardPending(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	runner.saveStatus(t.Context())

	if exhaustedWarnings() != 0 {
		t.Fatalf("discarded notifications still reported: %v", runner.status.Warnings)
	}
}

func TestKeepStatusAliveRefreshesSnapshotTime(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "repo.json")
	old := time.Now().Add(-time.Hour)

	err := os.WriteFile(path, []byte("{}"), 0o600)
	if err == nil {
		err = os.Chtimes(path, old, old)
	}

	if err != nil {
		t.Fatal(err)
	}

	stop := keepStatusAlive(path, slog.New(slog.DiscardHandler))

	time.Sleep(statusInterval + statusInterval/2)
	stop()

	info, err := os.Stat(path)
	if err != nil || time.Since(info.ModTime()) > statusInterval {
		t.Fatal("snapshot time not refreshed", info.ModTime(), err)
	}
}
