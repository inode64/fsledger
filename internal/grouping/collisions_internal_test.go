package grouping

import (
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/event"
)

const (
	collisionRoot  = "/tree"
	collisionChild = "/tree/file"
)

func TestDuplicateDetectionDoesNotDetachOverlappingPaths(t *testing.T) {
	t.Parallel()

	for range 64 {
		now := time.Now()
		group := &event.Group{Actor: event.Actor{Known: true}, First: now, Last: now, Paths: map[string]event.Raw{
			collisionRoot:  {Path: collisionRoot, Backend: event.Fanotify, Operation: event.Create, Time: now},
			collisionChild: {Path: collisionChild, Backend: event.Fanotify, Operation: event.Write, Time: now},
		}}
		manager := New(config.Grouping{}, time.Second, time.Minute)
		manager.groups["known"] = group
		manager.Add(event.Raw{Path: collisionChild, Backend: event.Inotify, Operation: event.Write, Time: now})

		if len(group.Paths) != 2 {
			t.Fatal("duplicate discarded previously detached parent", group.Paths)
		}
	}
}

func TestEnrichmentKeepsConflictDebounce(t *testing.T) {
	t.Parallel()

	now := time.Now()
	manager := New(
		config.Grouping{ByProcess: true, ByUser: false, PreferLoginUID: false},
		time.Second,
		time.Minute,
	)
	manager.groups[event.Unknown] = &event.Group{First: now.Add(-time.Minute), Last: now, Paths: map[string]event.Raw{
		collisionRoot: {
			Path:      collisionRoot,
			Backend:   event.Inotify,
			Operation: event.Write,
			Time:      now.Add(-time.Second),
		},
		collisionChild: {
			Path:      collisionChild,
			Backend:   event.Inotify,
			Operation: event.Write,
			Time:      now.Add(-time.Second),
		},
	}}
	manager.Enrich(func(raw event.Raw) event.Actor {
		pid := 1
		if raw.Path == collisionChild {
			pid = 2
		}

		return event.Actor{Known: true, PID: pid, StartTime: 1}
	})

	group := manager.groups[event.Unknown]
	if group == nil || group.Reason != conflictingActors || !group.Last.Equal(now) {
		t.Fatal("conflict enrichment shortened debounce", group)
	}
}
