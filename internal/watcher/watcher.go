// Package watcher defines nonblocking detector queues independently of Git.
package watcher

import (
	"context"
	"sync/atomic"

	"github.com/inode64/fsledger/internal/resource"

	"github.com/inode64/fsledger/internal/event"
)

// QueueSize bounds each native detector independently.
const QueueSize = 4096

// Watcher owns kernel resources until Close is called.
type Watcher interface {
	Start(ctx context.Context) error
	Events() <-chan *event.Raw
	Dirty() bool
	Close() error
	Name() string
}

// PollingRequired reports stopped readers or lost native coverage without consuming either signal.
func PollingRequired(done <-chan struct{}, coverageLost *atomic.Bool) bool {
	select {
	case <-done:
		return true
	default:
		return coverageLost.Load()
	}
}

// Queue never blocks a kernel reader; dropped events latch a reconciliation flag.
type Queue struct {
	// Channel carries pointers: every repository keeps several queues, and slots sized for whole events
	// would pin megabytes of idle heap in each of them.
	Channel   chan *event.Raw
	reason    atomic.Pointer[string]
	lost      atomic.Bool
	reconcile atomic.Bool
}

// NewQueue creates a bounded queue.
func NewQueue(size int) *Queue {
	return &Queue{
		Channel: make(chan *event.Raw, size), reason: atomic.Pointer[string]{},
		lost: atomic.Bool{}, reconcile: atomic.Bool{},
	}
}

// Send emits data events; control requests are latched once, independently of capacity.
func (q *Queue) Send(raw event.Raw) {
	if raw.Dirty {
		if raw.Reason == event.Reconciliation {
			q.reconcile.Store(true)
		} else {
			q.lose(raw.Reason)
		}

		resource.OptionalFile(raw.PIDFD)

		return
	}

	queued := raw

	select {
	case q.Channel <- &queued:
	default:
		resource.OptionalFile(raw.PIDFD)
		q.lose("event queue overflow")
	}
}

// LossReason consumes the cause of the loss reported by Dirty; control requests never enter Channel.
func (q *Queue) LossReason() string {
	reason := q.reason.Swap(nil)
	if reason == nil {
		return ""
	}

	return *reason
}

// Dirty consumes the sticky loss flag independently of queue capacity.
func (q *Queue) Dirty() bool { return q.lost.Swap(false) }

// ReconciliationPending consumes ordinary scan requests without reporting event loss.
func (q *Queue) ReconciliationPending() bool { return q.reconcile.Swap(false) }

// HasLoss reports loss without consuming the worker's recovery request.
func (q *Queue) HasLoss() bool { return q.lost.Load() }

// lose keeps the first cause: later losses are consequences the operator does not need.
func (q *Queue) lose(reason string) {
	q.reason.CompareAndSwap(nil, &reason)
	q.lost.Store(true)
}
