package daemon

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/watcher/fanotify"
)

const opsNotifier = "ops"

// announced returns the message of the single due delivery, which must carry the expected event.
func announced(t *testing.T, runner *worker, event string) catalog.Message {
	t.Helper()

	deliveries, err := runner.catalog.Due(t.Context())
	if err != nil || len(deliveries) != 1 || deliveries[0].Message.Event != event {
		t.Fatalf("%s not announced exactly once: %d %v", event, len(deliveries), err)
	}

	err = runner.catalog.DeliveredMany(t.Context(), []int64{deliveries[0].ID})
	if err != nil {
		t.Fatal(err)
	}

	return deliveries[0].Message
}

// The error alert must name the warnings that raised it, and the recovery alert what was recovered from.
func TestErrorAndRecoveryAnnouncementsCarryWarningReasons(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	runner.catalog.Notifications.Use = []string{opsNotifier}
	runner.catalog.Notifications.BatchWindow = 0

	runner.warn("fanotify event loss or coverage loss; reconciliation required")
	runner.warn("shutdown grace period exhausted")
	runner.announceError(t.Context())

	reason := announced(t, runner, config.NotificationError).Reason
	if !strings.Contains(reason, "coverage loss") || !strings.Contains(reason, "grace period") {
		t.Fatalf("error announced without its warnings: %q", reason)
	}

	err := runner.announceRecovery(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if reason = announced(t, runner, config.NotificationRecovery).Reason; !strings.Contains(reason, "coverage loss") {
		t.Fatalf("recovery does not say what it recovered from: %q", reason)
	}
}

// A later, unrelated error must not repeat reasons that were already recovered.
func TestErrorAnnouncementDropsRecoveredReasons(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	runner.catalog.Notifications.Use = []string{opsNotifier}
	runner.catalog.Notifications.BatchWindow = 0

	runner.warn("fanotify event loss or coverage loss; reconciliation required")
	runner.announceError(t.Context())
	_ = announced(t, runner, config.NotificationError)

	err := runner.announceRecovery(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	_ = announced(t, runner, config.NotificationRecovery)

	runner.warn("Git publication failed")
	runner.announceError(t.Context())

	reason := announced(t, runner, config.NotificationError).Reason
	if strings.Contains(reason, "coverage loss") || !strings.Contains(reason, "Git publication failed") {
		t.Fatalf("second error carries stale reasons: %q", reason)
	}
}

// Warnings raised since the last recovery leave the status when the repository recovers; permanent
// advisories (the kernel pidfd limitation) stay visible.
func TestRecoveryClearsWarningsButKeepsAdvisories(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)

	advisory := fanotify.PIDFDLifetime{Available: false, Checked: true, Reason: ""}
	runner.recordPIDFDLifetime(advisory)
	runner.warn("fanotify event loss or coverage loss; reconciliation required")

	if len(runner.status.Warnings) != 2 {
		t.Fatalf("advisory and warning expected: %v", runner.status.Warnings)
	}

	runner.announceError(t.Context())

	err := runner.announceRecovery(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{advisory.Warning()}; !slices.Equal(runner.status.Warnings, want) {
		t.Fatalf("recovered status still carries warnings: %v; want %v", runner.status.Warnings, want)
	}
}

// A storm of losses, each healed by its own reconciliation, is one incident: one error when it starts and
// one recovery once the quiet period passes without a relapse.
func TestRecoveryWaitsForQuietPeriod(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	runner.catalog.Notifications.Use = []string{opsNotifier}
	runner.catalog.Notifications.BatchWindow = 0
	runner.recoveryQuiet = time.Hour

	const loss = "event loss or watcher coverage loss (event queue overflow); reconciliation required"

	for range 3 {
		runner.warn(loss)
		runner.announceError(t.Context())

		err := runner.announceRecovery(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		if runner.pendingError || !runner.recovered || len(runner.status.Warnings) != 1 {
			t.Fatalf("healed loss not held as recovered: %v", runner.status.Warnings)
		}
	}

	_ = announced(t, runner, config.NotificationError)

	err := runner.settleRecovery(t.Context(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	if reason := announced(t, runner, config.NotificationRecovery).Reason; reason != loss {
		t.Fatalf("recovery does not say what it recovered from: %q", reason)
	}

	if runner.announcedError || runner.recovered || len(runner.status.Warnings) != 0 {
		t.Fatalf("settled recovery left state behind: %v", runner.status.Warnings)
	}
}

// A relapse inside the quiet period cancels the held recovery without a second error.
func TestRelapseCancelsHeldRecovery(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	runner.catalog.Notifications.Use = []string{opsNotifier}
	runner.catalog.Notifications.BatchWindow = 0
	runner.recoveryQuiet = time.Hour

	runner.warn("kernel loss")
	runner.announceError(t.Context())
	_ = announced(t, runner, config.NotificationError)

	err := runner.announceRecovery(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	runner.warn("kernel loss")
	runner.announceError(t.Context())

	err = runner.settleRecovery(t.Context(), time.Now().Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	deliveries, err := runner.catalog.Due(t.Context())
	if err != nil || len(deliveries) != 0 || !runner.pendingError || !runner.announcedError {
		t.Fatalf("relapse announced again or recovered early: %d %v", len(deliveries), err)
	}
}
