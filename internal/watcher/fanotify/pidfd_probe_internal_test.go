package fanotify

import (
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestReapedPIDFDRecord(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		pid       int
		pidfd     uint32
		wantError bool
	}{
		{name: "reaped writer", pidfd: math.MaxUint32, pid: 42, wantError: false},
		{name: "allocation failure is inconclusive", pidfd: math.MaxUint32 - 1, pid: 42, wantError: true},
		{name: "another actor is inconclusive", pidfd: math.MaxUint32, pid: 43, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			available, err := reapedPIDFDRecord(probeRecord(test.pidfd), test.pid)
			if available || (err != nil) != test.wantError {
				t.Fatalf("available=%v error=%v", available, err)
			}
		})
	}
}

//nolint:paralleltest // Parallel opens could reuse the closed descriptor before the assertion.
func TestReapedPIDFDRecordClosesDescriptor(t *testing.T) {
	fd, err := unix.Eventfd(0, unix.EFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}

	//nolint:gosec // The kernel allocated a nonnegative descriptor, encoded as a 32-bit ABI field.
	record := probeRecord(uint32(fd))

	available, err := reapedPIDFDRecord(record, 42)
	if err != nil || !available {
		t.Fatalf("available=%v error=%v", available, err)
	}

	_, err = unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if !errors.Is(err, unix.EBADF) {
		t.Fatalf("probe leaked descriptor: %v", err)
	}
}

func TestPIDFDLifetimeWarning(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		text   string
		result PIDFDLifetime
	}{
		{name: "corrected", result: PIDFDLifetime{Checked: true, Available: true, Reason: ""}, text: ""},
		{
			name: "missing fix", result: PIDFDLifetime{Checked: true, Available: false, Reason: ""},
			text: "kernel fix required",
		},
		{
			name:   "permission denied",
			result: PIDFDLifetime{Checked: false, Available: false, Reason: "operation not permitted"},
			text:   "status unknown",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			message := test.result.Warning()
			if test.text == "" {
				if message != "" {
					t.Fatal(message)
				}

				return
			}

			if !strings.Contains(message, test.text) || !strings.Contains(message, ReapedPIDFDFix) ||
				!strings.Contains(message, test.result.Reason) {
				t.Fatal(message)
			}
		})
	}
}

func probeRecord(pidfd uint32) []byte {
	const size = 32

	record := make([]byte, size)
	binary.NativeEndian.PutUint32(record, size)
	record[4] = unix.FANOTIFY_METADATA_VERSION
	binary.NativeEndian.PutUint16(record[6:8], metadataSize)
	binary.NativeEndian.PutUint64(record[8:16], unix.FAN_CLOSE_WRITE)
	binary.NativeEndian.PutUint32(record[16:20], math.MaxUint32)
	binary.NativeEndian.PutUint32(record[20:24], 42)
	record[24] = unix.FAN_EVENT_INFO_TYPE_PIDFD
	binary.NativeEndian.PutUint16(record[26:28], 8)
	binary.NativeEndian.PutUint32(record[28:32], pidfd)

	return record
}
