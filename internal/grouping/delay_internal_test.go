package grouping

import (
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/event"
)

func TestShortDebounceWaitsForAttribution(t *testing.T) {
	t.Parallel()

	manager := New(
		config.Grouping{ByProcess: true, ByUser: false, PreferLoginUID: false},
		time.Millisecond,
		2*time.Millisecond,
	)
	now := time.Now()
	manager.Add(event.Raw{Path: "/file", Time: now})

	if groups := manager.DueWithAttributionDelay(
		now.Add(10*time.Millisecond),
		false,
		250*time.Millisecond,
	); len(
		groups,
	) != 0 {
		t.Fatal("unresolved event drained before correlation window")
	}

	manager.Enrich(func(event.Raw) event.Actor { return event.Actor{Known: true, PID: 123, StartTime: 1} })

	groups := manager.DueWithAttributionDelay(now.Add(250*time.Millisecond), false, 250*time.Millisecond)
	if len(groups) != 1 || !groups[0].Actor.Known {
		t.Fatal("mature event lost delayed attribution", groups)
	}
}
