package daemon

import (
	"slices"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/watcher/fanotify"
)

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
	runner.catalog.Notifications.Use = []string{"ops"}
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
	runner.catalog.Notifications.Use = []string{"ops"}
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
