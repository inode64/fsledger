package fanotify

import (
	"context"
	"encoding/binary"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

// ReapedPIDFDFix documents the kernel change needed for queued events from reaped writers.
const ReapedPIDFDFix = "https://lore.kernel.org/linux-fsdevel/20260607003343.425939-1-anonymemeow@gmail.com/#t"

// PIDFDLifetime distinguishes a confirmed kernel limitation from an inconclusive probe.
// Success only tests descriptor delivery after reaping, not cgroup lookup or attribution.
type PIDFDLifetime struct {
	Reason    string `json:"reason,omitempty"`
	Available bool   `json:"available"`
	Checked   bool   `json:"checked"`
}

// Warning returns actionable diagnostics without inferring support from uname.
func (result PIDFDLifetime) Warning() string {
	if result.Checked && result.Available {
		return ""
	}

	if result.Checked {
		return "fanotify returned FAN_NOPIDFD for a reaped writer; short-lived writers may have unknown origin; " +
			"kernel fix required: " + ReapedPIDFDFix
	}

	return "cannot verify fanotify pidfds for reaped writers: " + result.Reason +
		"; kernel fix status unknown; see " + ReapedPIDFDFix
}

// ProbePIDFDLifetime writes one private temporary file beneath parent and removes it.
// Call only when fanotify PIDFD attribution is selected. The caller chooses a runtime
// directory, never a source path. A child is reaped BEFORE reading the queued event:
// opening a pidfd while the writer is alive would conceal the kernel bug.
func ProbePIDFDLifetime(ctx context.Context, parent string) PIDFDLifetime {
	available, err := probePIDFDLifetime(ctx, parent)
	if err != nil {
		return PIDFDLifetime{Available: false, Checked: false, Reason: err.Error()}
	}

	return PIDFDLifetime{Available: available, Checked: true, Reason: ""}
}

func probePIDFDLifetime(ctx context.Context, parent string) (bool, error) {
	fd, err := unix.FanotifyInit(unix.FAN_CLASS_NOTIF|unix.FAN_CLOEXEC|unix.FAN_NONBLOCK|unix.FAN_REPORT_PIDFD,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_LARGEFILE)
	if err != nil {
		return false, fault.Wrap("initialize pidfd probe", err)
	}
	defer resource.FD(fd)

	path, err := os.MkdirTemp(parent, ".fsledger-pidfd-*")
	if err != nil {
		return false, fault.Wrap("create pidfd probe directory", err)
	}
	defer removeProbe(path)

	err = unix.FanotifyMark(fd, unix.FAN_MARK_ADD, unix.FAN_CLOSE_WRITE|unix.FAN_EVENT_ON_CHILD, unix.AT_FDCWD, path)
	if err != nil {
		return false, fault.Wrap("mark pidfd probe directory", err)
	}

	const timeout = 2 * time.Second

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// The script is fixed; the private path is a quoted positional argument. Shell
	// builtins ensure that the writer is the child we reap, without another exec.
	//nolint:gosec // No input is interpolated into the script; the private path is passed as a quoted argument.
	child := exec.CommandContext(
		ctx,
		"/bin/sh",
		"-c",
		`printf probe > "$1"`,
		"fsledger-pidfd-probe",
		filepath.Join(path, "write"),
	)
	child.Env = []string{"PATH=/usr/bin:/bin"}

	err = child.Run()
	if err != nil {
		return false, fault.Wrap("run pidfd probe writer", err)
	}

	return readReapedPIDFD(fd, child.Process.Pid)
}

func removeProbe(path string) {
	err := os.RemoveAll(path)
	if err != nil {
		slog.Warn("pidfd probe cleanup failed", "error", err)
	}
}

func readReapedPIDFD(fd, pid int) (bool, error) {
	// This isolated FD-mode group emits exactly one CLOSE_WRITE with a PIDFD record.
	const recordSize = metadataSize + infoHeaderSize + 4

	buffer := make([]byte, recordSize)

	count, err := unix.Read(fd, buffer)
	if err != nil {
		return false, fault.Wrap("read reaped writer event", err)
	}

	return reapedPIDFDRecord(buffer[:count], pid)
}

func reapedPIDFDRecord(record []byte, pid int) (bool, error) {
	const recordSize = metadataSize + infoHeaderSize + 4
	if len(record) < metadataSize {
		return false, fault.New("unexpected pidfd probe event size")
	}

	descriptor := signed(record[16:20])
	if descriptor >= 0 {
		defer resource.FD(descriptor)
	}

	pidfd, err := readPIDFD(record[metadataSize:])
	if err != nil {
		return false, err
	}
	defer resource.OptionalFile(pidfd)

	if len(record) != recordSize || binary.NativeEndian.Uint32(record[:4]) != recordSize ||
		binary.NativeEndian.Uint16(record[6:8]) != metadataSize ||
		record[4] != unix.FANOTIFY_METADATA_VERSION || signed(record[20:24]) != pid ||
		binary.NativeEndian.Uint64(record[8:16]) != unix.FAN_CLOSE_WRITE ||
		record[metadataSize] != unix.FAN_EVENT_INFO_TYPE_PIDFD ||
		binary.NativeEndian.Uint16(record[26:28]) != infoHeaderSize+4 {
		return false, fault.New("unexpected pidfd probe event")
	}

	if pidfd != nil {
		return true, nil
	}

	if signed(record[metadataSize+infoHeaderSize:]) == unix.FAN_NOPIDFD {
		return false, nil
	}

	return false, fault.New("kernel could not allocate probe pidfd (FAN_EPIDFD)")
}
