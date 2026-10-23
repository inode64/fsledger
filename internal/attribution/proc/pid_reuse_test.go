package proc_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/attribution/proc"
	"github.com/inode64/fsledger/internal/event"
)

// tick is one Linux USER_HZ clock tick, the unit of /proc/<pid>/stat start times.
const tick = time.Second / 100

// readLater stamps an event read one second from now on CLOCK_BOOTTIME, as fanotify stamps
// the events it reads, so that the test process predates it even if it started this tick.
func readLater(t *testing.T) time.Duration {
	t.Helper()

	var now unix.Timespec

	err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &now)
	if err != nil {
		t.Fatal(err)
	}

	return time.Duration(now.Nano()) + time.Second
}

func TestResolveWithoutPIDFDRequiresStartBeforeEvent(t *testing.T) {
	t.Parallel()

	pid := os.Getpid()

	self := proc.Resolve(t.Context(), event.Raw{PID: pid, SinceBoot: readLater(t)})
	if !self.Known || self.StartTime == 0 {
		t.Fatalf("process started before the event was not attributed: %+v", self)
	}

	//nolint:gosec // Start ticks since boot stay far below the int64 range.
	started := time.Duration(self.StartTime) * tick

	for _, test := range []struct {
		name      string
		sinceBoot time.Duration
		known     bool
	}{
		{name: "unknown event time", sinceBoot: 0, known: false},
		{name: "event read in the start tick", sinceBoot: started, known: false},
		{name: "event read at the end of the start tick", sinceBoot: started + tick - 1, known: false},
		{name: "event read after the start tick", sinceBoot: started + tick, known: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			actor := proc.Resolve(t.Context(), event.Raw{PID: pid, SinceBoot: test.sinceBoot})
			if actor.Known != test.known || (!test.known && actor.UnavailableReason == "") {
				t.Fatalf("event at %v for a start at %v: %+v", test.sinceBoot, started, actor)
			}
		})
	}
}

func TestResolveDoesNotUseReapedWriterPID(t *testing.T) {
	t.Parallel()

	raw := event.Raw{PID: os.Getpid(), SinceBoot: readLater(t), Reaped: true}

	actor := proc.Resolve(t.Context(), raw)
	if actor.Known || !strings.Contains(actor.UnavailableReason, "FAN_NOPIDFD") {
		t.Fatalf("reaped writer attributed to the process now holding its PID: %+v", actor)
	}
}

func TestResolveWithPIDFDKeepsLivenessCheck(t *testing.T) {
	t.Parallel()

	descriptor, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		t.Skipf("pidfd unavailable: %v", err)
	}

	pidfd := os.NewFile(uintptr(descriptor), "pidfd")

	t.Cleanup(func() {
		closeErr := pidfd.Close()
		if closeErr != nil {
			t.Error(closeErr)
		}
	})

	// The pidfd pins the process, so the event's read time is not needed.
	actor := proc.Resolve(t.Context(), event.Raw{PID: os.Getpid(), PIDFD: pidfd})
	if !actor.Known {
		t.Fatalf("live pidfd process not attributed: %+v", actor)
	}
}
