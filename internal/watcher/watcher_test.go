package watcher_test

import (
	"testing"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/watcher"
)

func TestSaturationAndOverflowAreSticky(t *testing.T) {
	t.Parallel()

	queue := watcher.NewQueue(1)
	queue.Send(event.Raw{Path: "/a"})
	queue.Send(event.Raw{Path: "/b"})

	if !queue.Dirty() {
		t.Fatal("saturation was lost")
	}

	if queue.Dirty() {
		t.Fatal("dirty not consumed")
	}

	queue.Send(event.Raw{Dirty: true, Reason: "IN_Q_OVERFLOW"})

	queue.Send(event.Raw{Dirty: true, Reason: "later loss"})

	if reason := queue.LossReason(); reason != "event queue overflow" {
		t.Fatal("first loss reason not kept:", reason)
	}

	if !queue.Dirty() || queue.LossReason() != "" {
		t.Fatal("explicit overflow was lost behind full queue, or its reason was not consumed")
	}
}

func TestScheduledReconciliationIsNotLoss(t *testing.T) {
	t.Parallel()

	queue := watcher.NewQueue(1)
	for range 3 {
		queue.Send(event.Raw{Dirty: true, Reason: event.Reconciliation, Backend: event.Polling})
	}

	if queue.Dirty() || queue.HasLoss() {
		t.Fatal("ordinary polling reported overflow")
	}

	if !queue.ReconciliationPending() || queue.ReconciliationPending() {
		t.Fatal("coalesced scan request was lost or not consumed")
	}

	queue.Send(event.Raw{Path: "/first"})
	queue.Send(event.Raw{Path: "/lost"})

	if !queue.Dirty() {
		t.Fatal("real queue overflow was hidden")
	}
}
