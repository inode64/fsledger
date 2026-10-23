// Package watcher defines nonblocking detector queues independently of Git.
package watcher

import (
	"context"
	"maps"
	"slices"
	"sync"
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
	Channel chan *event.Raw
	reason  atomic.Pointer[string]
	// dropped remembers, up to remember paths, those whose events a full queue dropped.
	dropped   map[string]struct{}
	remember  int
	droppedMu sync.Mutex
	lost      atomic.Bool
	reconcile atomic.Bool
}

// NewQueue creates a bounded queue whose overflow is a loss.
func NewQueue(size int) *Queue {
	return &Queue{
		Channel: make(chan *event.Raw, size), reason: atomic.Pointer[string]{},
		dropped: nil, remember: 0, droppedMu: sync.Mutex{},
		lost: atomic.Bool{}, reconcile: atomic.Bool{},
	}
}

// NewRememberingQueue creates a bounded queue that keeps the paths of up to remember dropped events for
// Dropped; only beyond them is an overflow a loss.
func NewRememberingQueue(size, remember int) *Queue {
	queue := NewQueue(size)
	queue.dropped, queue.remember = make(map[string]struct{}), remember

	return queue
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

		if !q.rememberDropped(raw.Path, raw.OldPath) {
			q.lose("event queue overflow")
		}
	}
}

// Dropped consumes the paths whose events a full queue dropped: their actors are unknown.
func (q *Queue) Dropped() []string {
	q.droppedMu.Lock()
	defer q.droppedMu.Unlock()

	paths := slices.Collect(maps.Keys(q.dropped))
	clear(q.dropped)

	return paths
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

func (q *Queue) rememberDropped(paths ...string) bool {
	if q.remember == 0 {
		return false
	}

	q.droppedMu.Lock()
	defer q.droppedMu.Unlock()

	for _, path := range paths {
		if path == "" {
			continue
		}

		if _, known := q.dropped[path]; !known && len(q.dropped) >= q.remember {
			return false
		}

		q.dropped[path] = struct{}{}
	}

	return true
}

// lose keeps the first cause: later losses are consequences the operator does not need.
func (q *Queue) lose(reason string) {
	q.reason.CompareAndSwap(nil, &reason)
	q.lost.Store(true)
}
