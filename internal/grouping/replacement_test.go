package grouping_test

import (
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/grouping"
)

const replacedPath = "/etc/f"

func TestInotifyCreateAfterAttributedWrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		delay time.Duration
		known bool
	}{
		{"creation before write", -10 * time.Millisecond, true},
		{"simultaneous creation", 0, true},
		{"rename over by another process", 100 * time.Millisecond, false},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			manager := grouping.New(policy(), time.Second, 10*time.Second)
			now := time.Now()

			for _, raw := range []event.Raw{
				{Path: replacedPath, Time: now, Backend: event.Inotify, Operation: event.Write},
				{Path: replacedPath, Time: now, Actor: actor(1, 1, 1000), Backend: event.Fanotify, Operation: event.Write},
				{Path: replacedPath, Time: now.Add(testCase.delay), Backend: event.Inotify, Operation: event.Create},
			} {
				manager.Add(raw)
			}

			groups := manager.Due(now.Add(time.Minute), true)
			if len(groups) != 1 || groups[0].Actor.Known != testCase.known {
				t.Fatalf("creation %v after the write: want known=%v, got %+v", testCase.delay, testCase.known, groups)
			}
		})
	}
}
