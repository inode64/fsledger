package fanotify

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

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
