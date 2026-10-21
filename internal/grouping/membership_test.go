package grouping_test

import (
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/grouping"
)

const (
	membershipDir  = "/etc/app"
	membershipFile = "/etc/app/a.conf"
)

func TestConflictMovesOwnOverlapsToUnknown(t *testing.T) {
	t.Parallel()

	manager := grouping.New(policy(), time.Second, time.Minute)
	now := time.Now()
	owner, intruder, later := actor(1, 1, 1000), actor(2, 2, 1000), actor(3, 3, 1000)

	for _, raw := range []event.Raw{
		{Path: membershipDir, Time: now, Actor: owner, Backend: event.Fanotify, Operation: event.Create},
		{Path: membershipFile, Time: now, Actor: owner, Backend: event.Fanotify, Operation: event.Write},
		{Path: "/etc/app/b.conf", Time: now.Add(10 * time.Millisecond), Actor: intruder, Backend: event.Fanotify},
		{Path: membershipFile, Time: now.Add(500 * time.Millisecond), Actor: owner, Backend: event.Fanotify},
	} {
		manager.Add(raw)
	}

	if groups, paths := manager.Pending(); groups != 1 || paths != 3 {
		t.Fatalf("conflicting path kept in two groups: groups=%d paths=%d", groups, paths)
	}

	manager.Add(event.Raw{Path: "/srv/other", Time: now.Add(1400 * time.Millisecond), Actor: owner})

	if groups := manager.Due(now.Add(1600*time.Millisecond), false); len(groups) != 1 || groups[0].Actor.Known {
		t.Fatalf("conflict group did not drain first: %+v", groups)
	}

	manager.Add(event.Raw{Path: membershipFile, Time: now.Add(1700 * time.Millisecond), Actor: later})

	for _, group := range manager.Due(now.Add(5*time.Second), true) {
		if _, found := group.Paths[membershipFile]; found && group.Actor.PID != later.PID {
			t.Fatalf("write by PID %d attributed to PID %d", later.PID, group.Actor.PID)
		}
	}
}
