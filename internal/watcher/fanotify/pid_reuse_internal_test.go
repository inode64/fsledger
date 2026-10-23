package fanotify

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/resource"
	"github.com/inode64/fsledger/internal/watcher"
)

func TestReadPIDFDReportsReapedWriter(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		pidfd  uint32
		reaped bool
	}{
		{name: "FAN_NOPIDFD", pidfd: math.MaxUint32, reaped: true},
		{name: "FAN_EPIDFD", pidfd: math.MaxUint32 - 1, reaped: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			pidfd, reaped, err := readPIDFD(probeRecord(test.pidfd)[metadataSize:])
			if err != nil || pidfd != nil || reaped != test.reaped {
				t.Fatalf("pidfd=%v reaped=%v error=%v", pidfd, reaped, err)
			}
		})
	}
}

func TestDescriptorEventRecordsReadTimeAndReapedWriter(t *testing.T) {
	t.Parallel()

	file, err := os.Create(filepath.Join(t.TempDir(), "written"))
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(file)

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector := new(Watcher)

	detector.queue, detector.matcher, detector.path = watcher.NewQueue(1), matcher, filepath.Dir(file.Name())

	var before unix.Timespec

	err = unix.ClockGettime(unix.CLOCK_BOOTTIME, &before)
	if err != nil {
		t.Fatal(err)
	}

	// handle closes the event descriptor, as it closes those the kernel opens.
	descriptor, err := unix.Dup(int(file.Fd()))
	if err != nil {
		t.Fatal(err)
	}

	// A CLOSE_WRITE record from PID 42 whose PIDFD record carries FAN_NOPIDFD.
	record := probeRecord(math.MaxUint32)
	//nolint:gosec // The kernel allocated a nonnegative descriptor, encoded as a 32-bit ABI field.
	binary.NativeEndian.PutUint32(record[16:20], uint32(descriptor))

	err = detector.handle(record, metadataSize)
	if err != nil {
		t.Fatal(err)
	}

	raw := <-detector.queue.Channel
	if raw.Path != file.Name() || raw.PID != 42 || !raw.Reaped || raw.SinceBoot.Nanoseconds() < before.Nano() {
		t.Fatalf("event lost the evidence that its PID may be reused: %+v", raw)
	}
}

// A backlog of queued events must not hold one pidfd each: the daemon would run out of descriptors.
func TestBacklogDropsNewPIDFDs(t *testing.T) {
	t.Parallel()

	file, err := os.Create(filepath.Join(t.TempDir(), "file"))
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(file)

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector := new(Watcher)
	detector.queue, detector.matcher = watcher.NewQueue(pidfdBacklog+1), matcher
	detector.path = filepath.Dir(file.Name())

	for range pidfdBacklog {
		detector.queue.Send(event.Raw{Path: file.Name()})
	}

	// handle closes both the event descriptor and a dropped pidfd, as it closes those the kernel opens.
	descriptors := make([]int, 2)
	for index := range descriptors {
		descriptors[index], err = unix.Dup(int(file.Fd()))
		if err != nil {
			t.Fatal(err)
		}
	}

	//nolint:gosec // The kernel allocated nonnegative descriptors, encoded as 32-bit ABI fields.
	record := probeRecord(uint32(descriptors[1]))
	//nolint:gosec // As above.
	binary.NativeEndian.PutUint32(record[16:20], uint32(descriptors[0]))

	err = detector.handle(record, metadataSize)
	if err != nil {
		t.Fatal(err)
	}

	var last *event.Raw
	for len(detector.queue.Channel) > 0 {
		last = <-detector.queue.Channel
	}

	if last == nil || last.Path != file.Name() || last.PIDFD != nil {
		t.Fatal("event over the backlog kept its pidfd", last)
	}
}
