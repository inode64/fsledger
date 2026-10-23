package proc

import (
	"time"

	"github.com/inode64/fsledger/internal/event"
)

// userHZ is the fixed Linux USER_HZ in which /proc/<pid>/stat start times are counted.
const userHZ = 100

// bootTime derives /proc/stat btime from the current clocks, without reading procfs. Derived for each call,
// it follows a step of the wall clock, which a cached boot time would miss for every later start time.
func bootTime() (time.Time, bool) {
	sinceBoot := event.SinceBoot()
	if sinceBoot == 0 {
		return time.Time{}, false
	}

	// The kernel truncates btime to whole seconds, as ps and the 1 s margin of ResolveAt expect.
	return time.Now().Round(0).Add(-sinceBoot).Truncate(time.Second), true
}

// StartTime converts a process start time in clock ticks since boot, as stored in event.Actor,
// to wall-clock time. It reports false when the start time or the boot time is unknown.
func StartTime(ticks uint64) (time.Time, bool) {
	boot, known := bootTime()
	if !known {
		return time.Time{}, false
	}

	return startTime(ticks, boot)
}

// startedBefore reports whether a start time in clock ticks since boot provably precedes an event
// read at sinceBoot on CLOCK_BOOTTIME. The kernel truncates start times to whole ticks, so a start
// in the event's own tick stays unproven, as does an unknown event time.
func startedBefore(ticks uint64, sinceBoot time.Duration) bool {
	if sinceBoot <= 0 {
		return false
	}

	return ticks < uint64(sinceBoot/(time.Second/userHZ))
}

func startTime(ticks uint64, boot time.Time) (time.Time, bool) {
	if ticks == 0 {
		return time.Time{}, false
	}

	//nolint:gosec // Ticks since boot stay far below the int64 range of a duration in ticks.
	return boot.Add(time.Duration(ticks) * (time.Second / userHZ)), true
}
