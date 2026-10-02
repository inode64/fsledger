package grouping_test

import (
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/grouping"
)

func actor(pid int, start uint64, uid uint32) event.Actor {
	return event.Actor{
		PID:        pid,
		StartTime:  start,
		UID:        uid,
		EUID:       0,
		LoginUID:   uid,
		Known:      true,
		UserKnown:  true,
		LoginKnown: true,
	}
}

func policy() config.Grouping {
	return config.Grouping{ByProcess: true, ByUser: true, PreferLoginUID: true}
}

func TestIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		first, second event.Actor
		groups        int
	}{
		{"same", actor(10, 100, 1000), actor(10, 100, 1000), 1},
		{"different user", actor(10, 100, 1000), actor(10, 100, 1001), 2},
		{"different process", actor(10, 100, 1000), actor(11, 100, 1000), 2},
		{"reused PID", actor(10, 100, 1000), actor(10, 200, 1000), 2},
		{event.Unknown, event.Actor{}, event.Actor{}, 1},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			manager := grouping.New(policy(), time.Second, 10*time.Second)
			now := time.Now()
			manager.Add(event.Raw{Path: "/a", Time: now, Actor: testCase.first})
			manager.Add(event.Raw{Path: "/b", Time: now, Actor: testCase.second})

			groups, paths := manager.Pending()
			if groups != testCase.groups || paths != 2 {
				t.Fatalf("groups=%d paths=%d", groups, paths)
			}
		})
	}
}

func TestDebounceAndMaximum(t *testing.T) {
	t.Parallel()

	manager := grouping.New(policy(), 2*time.Second, 5*time.Second)

	now := time.Now()
	for second := range 5 {
		manager.Add(event.Raw{Path: "/a", Time: now.Add(time.Duration(second) * time.Second)})

		if groups := manager.Due(now.Add(time.Duration(second)*time.Second), false); len(groups) != 0 {
			t.Fatal("premature flush")
		}
	}

	if len(manager.Due(now.Add(5*time.Second), false)) != 1 {
		t.Fatal("max delay did not expire")
	}

	manager.Add(event.Raw{Path: "/a", Time: now})

	if len(manager.Due(now.Add(2*time.Second), false)) != 1 {
		t.Fatal("debounce did not expire")
	}
}

func TestDelayedAttributionPreservesDeadlines(t *testing.T) {
	t.Parallel()

	manager := grouping.New(policy(), time.Second, 3*time.Second)
	first := time.Now()
	manager.Add(event.Raw{Path: "/a", Time: first})
	manager.Add(event.Raw{Path: "/a", Time: first.Add(2*time.Second + 500*time.Millisecond)})
	manager.Enrich(func(event.Raw) event.Actor { return actor(10, 100, 1000) })

	if groups := manager.Due(first.Add(2*time.Second+800*time.Millisecond), false); len(groups) != 0 {
		t.Fatal("enrichment shortened debounce")
	}

	if groups := manager.Due(first.Add(3*time.Second), false); len(groups) != 1 {
		t.Fatal("enrichment extended max_delay")
	}
}

func TestOverlappingActorsBecomeUnknown(t *testing.T) {
	t.Parallel()

	manager := grouping.New(policy(), time.Second, 10*time.Second)
	manager.Add(event.Raw{Path: "/a", Actor: actor(1, 1, 1000), Backend: event.Fanotify, Operation: event.Write})
	manager.Add(event.Raw{Path: "/a/b", Actor: actor(2, 2, 1001), Backend: event.Fanotify, Operation: event.Write})

	groups := manager.Due(time.Now(), true)
	if len(groups) != 1 || groups[0].Actor.Known || len(groups[0].Paths) != 2 {
		t.Fatalf("unsafe overlap grouping: %+v", groups)
	}
}

func TestDetectorEnrichmentBothOrders(t *testing.T) {
	t.Parallel()

	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "inotify first", true: "fanotify first"}[reverse], func(t *testing.T) {
			t.Parallel()

			manager := grouping.New(policy(), time.Second, 10*time.Second)

			events := []event.Raw{
				{Path: "/a", Backend: event.Inotify, Operation: event.Write},
				{Path: "/a", Backend: event.Fanotify, Operation: event.Write, Actor: actor(1, 1, 1000)},
			}
			if reverse {
				events[0], events[1] = events[1], events[0]
			}

			for _, raw := range events {
				manager.Add(raw)
			}

			groups := manager.Due(time.Now(), true)
			if len(groups) != 1 || !groups[0].Actor.Known {
				t.Fatalf("lost fanotify attribution: %+v", groups)
			}
		})
	}
}

func TestDisabledProcessGroupingDoesNotInventAProcess(t *testing.T) {
	t.Parallel()

	settings := policy()
	settings.ByProcess = false
	manager := grouping.New(settings, time.Second, 10*time.Second)
	manager.Add(event.Raw{Path: "/a", Actor: actor(1, 1, 1000)})
	manager.Add(event.Raw{Path: "/b", Actor: actor(2, 2, 1000)})

	groups := manager.Due(time.Now(), true)
	if len(groups) != 1 || !groups[0].Actor.Known || groups[0].Actor.PID != 0 || !groups[0].Actor.UserKnown {
		t.Fatalf("unsafe user-only grouping: %+v", groups)
	}
}

func TestOutOfOrderNotificationsDoNotShortenDebounce(t *testing.T) {
	t.Parallel()

	manager := grouping.New(policy(), time.Second, 10*time.Second)
	now := time.Now()
	manager.Add(event.Raw{Path: "/a", Time: now.Add(time.Second)})
	manager.Add(event.Raw{Path: "/a", Time: now})

	if len(manager.Due(now.Add(time.Second), false)) != 0 {
		t.Fatal("older event shortened debounce")
	}
}
