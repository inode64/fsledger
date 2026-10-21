package mountinfo

import (
	"errors"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

// Changes waits for mount table changes: the kernel flags /proc/self/mountinfo with POLLPRI after each
// mount, unmount or move in the namespace, so waiting needs neither a periodic sweep nor a timeout.
type Changes struct {
	stopping sync.Once
	table    int
	// Stop closes hangup, the write end of a pipe; its read end, wake, then ends Wait.
	wake, hangup int
}

// FollowChanges records the current table generation. A table read afterwards cannot miss a change:
// at worst the next Wait reports one that read already saw.
func FollowChanges() (*Changes, error) {
	table, err := unix.Open(tablePath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fault.Wrap("follow mount table", err)
	}

	var pipe [2]int

	err = unix.Pipe2(pipe[:], unix.O_CLOEXEC)
	if err != nil {
		resource.FD(table)

		return nil, fault.Wrap("create mount table wakeup", err)
	}

	return &Changes{stopping: sync.Once{}, table: table, wake: pipe[0], hangup: pipe[1]}, nil
}

// Wait blocks until the table changes after FollowChanges or the previous Wait, or until Stop; it
// reports a change. The kernel flags each change once, so a caller looping on Wait cannot spin.
func (c *Changes) Wait() (bool, error) {
	//nolint:gosec // Linux supplies bounded descriptors.
	fds := []unix.PollFd{{Fd: int32(c.table), Events: unix.POLLPRI}, {Fd: int32(c.wake), Events: unix.POLLIN}}

	_, err := unix.Poll(fds, -1)
	for errors.Is(err, unix.EINTR) {
		_, err = unix.Poll(fds, -1)
	}

	switch {
	case err != nil:
		return false, fault.Wrap("wait for mount table changes", err)
	case fds[1].Revents != 0:
		return false, nil
	case fds[0].Revents&unix.POLLPRI != 0:
		return true, nil
	default:
		return false, fault.New("mount table cannot be polled")
	}
}

// Stop ends the pending Wait and every later one; it may run concurrently with Wait.
func (c *Changes) Stop() { c.stopping.Do(func() { resource.FD(c.hangup) }) }

// Close stops waiting and releases the descriptors; no Wait may still be running.
func (c *Changes) Close() error {
	c.Stop()

	return fault.Wrap("close mount table", errors.Join(unix.Close(c.table), unix.Close(c.wake)))
}
