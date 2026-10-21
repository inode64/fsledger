package proc

import (
	"sync"
	"time"

	"github.com/prometheus/procfs"
)

// userHZ is the fixed Linux USER_HZ in which /proc/<pid>/stat start times are counted.
const userHZ = 100

// bootTime is read once: /proc/stat btime is constant while the system runs.
//
//nolint:gochecknoglobals // Process-wide constant of the running kernel, cached on first use.
var bootTime = sync.OnceValue(func() uint64 {
	fs, err := procfs.NewDefaultFS()
	if err != nil {
		return 0
	}

	stat, err := fs.Stat()
	if err != nil {
		return 0
	}

	return stat.BootTime
})

// StartTime converts a process start time in clock ticks since boot, as stored in event.Actor,
// to wall-clock time. It reports false when the start time or the boot time is unknown.
func StartTime(ticks uint64) (time.Time, bool) {
	return startTime(ticks, bootTime())
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

func startTime(ticks, boot uint64) (time.Time, bool) {
	if ticks == 0 || boot == 0 {
		return time.Time{}, false
	}

	seconds := boot + ticks/userHZ
	fraction := time.Duration(ticks%userHZ) * (time.Second / userHZ)

	//nolint:gosec // Boot time plus uptime in seconds stays far below the int64 range.
	return time.Unix(int64(seconds), int64(fraction)), true
}
