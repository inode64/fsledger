//nolint:paralleltest,testpackage // Keep kernel ABI measurements sequential, before changing production attribution.
package fanotify

import (
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/resource"
)

const kernelCgroupRoot = "/sys/fs/cgroup"

// TestKernelCgroup is the S1 feasibility gate. It deliberately fails if a reaped
// writer cannot be attributed. A successful ioctl on a previously acquired pidfd
// alone is not sufficient: fanotify must deliver that pidfd in the first place.
// Only private temporary directories, cgroups and notification groups are used.
func TestKernelCgroup(t *testing.T) {
	if os.Getenv("FSLEDGER_KERNEL_CGROUP") != "1" {
		t.Skip("opt in with FSLEDGER_KERNEL_CGROUP=1; needs root, cgroup v2 and pidfd info")
	}

	if os.Geteuid() != 0 {
		t.Skip("kernel cgroup experiment requires root")
	}

	for _, mode := range []struct {
		name  string
		flags uint
	}{
		{name: ModeFD, flags: 0},
		{name: ModeDFIDName, flags: unix.FAN_REPORT_FID | unix.FAN_REPORT_DFID_NAME},
	} {
		t.Run(mode.name, func(t *testing.T) {
			cgroup := kernelTestCgroup(t)
			t.Run("held_pidfd_after_reap", func(t *testing.T) {
				kernelCgroupHeld(t, cgroup, mode.flags)
			})
			t.Run("short_writers_concurrent_read", func(t *testing.T) {
				kernelCgroupWriters(t, cgroup, mode.flags, false)
			})
			t.Run("short_writers_read_after_reap", func(t *testing.T) {
				kernelCgroupWriters(t, cgroup, mode.flags, true)
			})
			t.Run("queued_mixed_cgroups", func(t *testing.T) {
				kernelCgroupMixed(t, cgroup, mode.flags)
			})
		})
	}
}

func kernelCgroupHeld(t *testing.T, cgroup *os.File, mode uint) {
	t.Helper()

	source := t.TempDir()
	group := kernelCgroupFanotify(t, source, mode)
	command := kernelCgroupWriter(t, cgroup, source, true)

	gate, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}

	err = command.Start()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if command.ProcessState == nil {
			resource.Close(gate)

			waitErr := command.Wait()
			if waitErr != nil {
				t.Errorf("reap held writer: %v", waitErr)
			}
		}
	})

	pidfd := kernelCgroupEvent(t, group)
	if pidfd < 0 {
		t.Fatalf("live writer returned pidfd=%d", pidfd)
	}
	defer resource.FD(pidfd)

	_, err = gate.Write([]byte("release\n"))
	if err != nil {
		t.Fatal(err)
	}

	err = command.Wait()
	if err != nil {
		t.Fatal(err)
	}

	kernelCgroupInfo(t, pidfd, cgroup.Name())
	t.Log("pidfd acquired while writer lived retains cgroup and exit information after Wait")
}

func kernelTestCgroup(t *testing.T) *os.File {
	t.Helper()

	_, err := os.Stat(filepath.Join(kernelCgroupRoot, "cgroup.controllers"))
	if err != nil {
		t.Skipf("unified cgroup hierarchy unavailable: %v", err)
	}

	//nolint:usetesting // A cgroup must be created inside cgroupfs, not TMPDIR.
	path, err := os.MkdirTemp(kernelCgroupRoot, "fsledger-kernel-test-")
	if err != nil {
		t.Skipf("cannot create private cgroup: %v", err)
	}

	t.Cleanup(func() {
		removeErr := os.Remove(path)
		if removeErr != nil {
			t.Errorf("remove private cgroup: %v", removeErr)
		}
	})

	//nolint:gosec // This is the private cgroup just created by this test.
	directory, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.Close(directory) })

	return directory
}

func kernelCgroupFanotify(t *testing.T, source string, mode uint) int {
	t.Helper()

	flags := uint(unix.FAN_CLASS_NOTIF|unix.FAN_CLOEXEC|unix.FAN_NONBLOCK|unix.FAN_REPORT_PIDFD) | mode

	group, err := unix.FanotifyInit(flags, unix.O_RDONLY|unix.O_CLOEXEC)
	if err != nil {
		t.Skipf("privileged fanotify with FAN_REPORT_PIDFD unavailable: %v", err)
	}

	t.Cleanup(func() { resource.FD(group) })

	// One close event per writer keeps missing pidfds distinct from queue losses
	// and coalescing. Both FD and DFID_NAME modes exercise the same workload.
	err = unix.FanotifyMark(group, unix.FAN_MARK_ADD,
		unix.FAN_CLOSE_WRITE|unix.FAN_EVENT_ON_CHILD, unix.AT_FDCWD, source)
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENODEV) || errors.Is(err, unix.EINVAL) {
		t.Skipf("filesystem does not support the requested fanotify mode: %v", err)
	}

	if err != nil {
		t.Fatal(err)
	}

	return group
}

func kernelCgroupWriter(t *testing.T, cgroup *os.File, source string, held bool) *exec.Cmd {
	t.Helper()

	script := `printf fixture > "$1"`
	if held {
		script += `; read -r release || :`
	}

	//nolint:gosec // The script is constant; the temporary path is passed as a quoted positional argument.
	command := exec.CommandContext(t.Context(), "/bin/sh", "-c", script, "cgroup-writer", filepath.Join(source, "file"))
	command.SysProcAttr = &syscall.SysProcAttr{}
	command.SysProcAttr.UseCgroupFD = true
	command.SysProcAttr.CgroupFD = int(cgroup.Fd())

	return command
}

//nolint:gocognit // Compare the same workload with and without waiting before reading.
func kernelCgroupWriters(t *testing.T, cgroup *os.File, mode uint, reapFirst bool) {
	t.Helper()

	const writers = 1000

	source := t.TempDir()
	group := kernelCgroupFanotify(t, source, mode)
	missing := 0

	for range writers {
		command := kernelCgroupWriter(t, cgroup, source, false)

		done := make(chan error, 1)
		go func() { done <- command.Run() }()

		if reapFirst {
			err := <-done
			if err != nil {
				t.Fatal(err)
			}
		}

		pidfd := kernelCgroupEvent(t, group)

		if !reapFirst {
			err := <-done
			if err != nil {
				t.Fatal(err)
			}
		}

		if pidfd == unix.FAN_NOPIDFD {
			missing++

			continue
		}

		if pidfd < 0 {
			t.Fatalf("pidfd creation failed: %d", pidfd)
		}

		func() {
			defer resource.FD(pidfd)

			kernelCgroupInfo(t, pidfd, cgroup.Name())
		}()
	}

	t.Logf("writers=%d FAN_NOPIDFD=%d (%.2f%%) resolved=%d reap_before_read=%t",
		writers, missing, 100*float64(missing)/writers, writers-missing, reapFirst)

	if reapFirst && missing != 0 {
		t.Fatalf(
			"S1 gate failed: %d/%d reaped writers have no pidfd; ioctl cannot recover their cgroup",
			missing,
			writers,
		)
	}
}

func kernelCgroupEvent(t *testing.T, group int) int {
	t.Helper()

	batch := kernelCgroupBatch(t, group)
	if len(batch) != 1 {
		for _, fd := range batch {
			if fd >= 0 {
				resource.FD(fd)
			}
		}

		t.Fatalf("expected one close-write event, received %d", len(batch))
	}

	return batch[0]
}

func kernelCgroupWait(t *testing.T, group int) {
	t.Helper()

	const timeoutMilliseconds = 5000
	//nolint:gosec // FanotifyInit returns a Linux signed int descriptor.
	poll := []unix.PollFd{{Fd: int32(group), Events: unix.POLLIN}}
	for {
		ready, err := unix.Poll(poll, timeoutMilliseconds)
		if errors.Is(err, unix.EINTR) {
			continue
		}

		if err != nil || ready != 1 || poll[0].Revents != unix.POLLIN {
			t.Fatalf("wait for writer event: ready=%d poll=%+v error=%v", ready, poll, err)
		}

		break
	}
}

func kernelCgroupBatch(t *testing.T, group int) []int {
	t.Helper()
	kernelCgroupWait(t, group)

	// Bound simultaneously installed file descriptors even for the queued workload.
	const batchBytes = 4096

	buffer := make([]byte, batchBytes)

	count, err := unix.Read(group, buffer)
	if err != nil || count < metadataSize {
		t.Fatalf("read writer event: count=%d error=%v", count, err)
	}

	var descriptors []int

	for data := buffer[:count]; len(data) > 0; {
		if len(data) < metadataSize {
			t.Fatal("truncated kernel event metadata")
		}

		length := int(binary.NativeEndian.Uint32(data[:4]))

		metadata := int(binary.NativeEndian.Uint16(data[6:8]))
		if length > len(data) || metadata < metadataSize || length < metadata {
			t.Fatal("invalid kernel event length")
		}

		descriptor := signed(data[16:20])
		if descriptor >= 0 {
			resource.FD(descriptor)
		}

		if binary.NativeEndian.Uint64(data[8:16]) != unix.FAN_CLOSE_WRITE {
			t.Fatal("expected a close-write event, not a queue overflow")
		}

		descriptors = append(descriptors, kernelCgroupPIDFD(t, data[metadata:length]))
		data = data[length:]
	}

	return descriptors
}

func kernelCgroupPIDFD(t *testing.T, info []byte) int {
	t.Helper()

	for len(info) >= infoHeaderSize {
		length := int(binary.NativeEndian.Uint16(info[2:4]))
		if length < infoHeaderSize || length > len(info) {
			t.Fatal("invalid kernel info record length")
		}

		if info[0] == unix.FAN_EVENT_INFO_TYPE_PIDFD && length == 8 {
			return signed(info[4:8])
		}

		info = info[length:]
	}

	t.Fatal("kernel event has no PIDFD info record")

	return unix.FAN_EPIDFD
}

func kernelCgroupMixed(t *testing.T, first *os.File, mode uint) {
	t.Helper()

	const writers = 1000

	cgroups := []*os.File{first, kernelTestCgroup(t)}
	source := t.TempDir()

	group := kernelCgroupFanotify(t, source, mode)
	for index := range writers {
		command := kernelCgroupWriter(t, cgroups[index%len(cgroups)], source, false)

		err := command.Run()
		if err != nil {
			t.Fatal(err)
		}
	}

	// Every writer has been reaped before the first read. Check all queued
	// events, even after a missing pidfd, and close each returned descriptor.
	observed, missing := 0, 0
	for observed < writers {
		batch := kernelCgroupBatch(t, group)
		for _, fd := range batch {
			switch {
			case fd == unix.FAN_NOPIDFD:
				missing++
			case fd < 0:
				t.Errorf("pidfd allocation failed: %d", fd)
			default:
				func() {
					defer resource.FD(fd)

					kernelCgroupInfo(t, fd, cgroups[observed%len(cgroups)].Name())
				}()
			}

			observed++
		}
	}

	t.Logf("queued_writers=%d cgroups=%d FAN_NOPIDFD=%d", observed, len(cgroups), missing)

	if missing != 0 || observed != writers {
		t.Fatalf("mixed-origin backlog lost attribution: observed=%d missing=%d", observed, missing)
	}
}

func kernelCgroupInfo(t *testing.T, pidfd int, expected string) {
	t.Helper()

	info := unix.PidfdInfo{}
	info.Mask = unix.PIDFD_INFO_CGROUPID | unix.PIDFD_INFO_EXIT

	err := unix.IoctlPidfdInfo(pidfd, &info)
	if errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EOPNOTSUPP) {
		t.Skipf("PIDFD_GET_INFO unavailable: %v", err)
	}

	if err != nil {
		t.Fatalf("PIDFD_GET_INFO fd=%d: %v", pidfd, err)
	}

	const required = unix.PIDFD_INFO_CGROUPID | unix.PIDFD_INFO_EXIT
	if info.Mask&required != required || info.Cgroupid == 0 || info.Exit_code != 0 {
		t.Fatalf("missing post-reap information: %+v", info)
	}

	anchor, err := unix.Open(kernelCgroupRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.FD(anchor)

	// kernfs encodes its 64-bit id in native byte order as FILEID_KERNFS (0xfe).
	const kernfsHandleType = 0xfe

	encoded := make([]byte, 8)
	binary.NativeEndian.PutUint64(encoded, info.Cgroupid)

	fd, err := unix.OpenByHandleAt(anchor, unix.NewFileHandle(kernfsHandleType, encoded), unix.O_PATH|unix.O_CLOEXEC)
	if err != nil {
		t.Fatalf("open cgroup handle id=%d anchor=%d: %v", info.Cgroupid, anchor, err)
	}
	defer resource.FD(fd)

	path, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil || path != expected {
		t.Fatalf("resolve cgroup id=%d: got=%q want=%q error=%v", info.Cgroupid, path, expected, err)
	}
}
