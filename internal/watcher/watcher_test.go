package watcher_test

import (
	"reflect"
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

// Every repository keeps several queues for its whole life: an idle slot must cost a word, not a whole
// event, or a host with many repositories fills its memory limit with empty buffers.
func TestIdleQueueSlotsHoldPointers(t *testing.T) {
	t.Parallel()

	queue := watcher.NewQueue(1)

	slot := reflect.TypeOf(queue.Channel).Elem().Size()
	if word := reflect.TypeFor[uintptr]().Size(); slot > word {
		t.Fatalf("an idle queue slot holds %d bytes; want at most %d", slot, word)
	}
}
