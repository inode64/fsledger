// Package proc enriches observed PIDs using procfs; disappearing processes are normal.
package proc

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/procfs"
	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/fault"
)

// Provider owns no process-global cache, avoiding stale identities after PID reuse.
type Provider struct{}

// Resolve reads start time before and after enrichment and checks pidfd liveness.
func (provider Provider) Resolve(ctx context.Context, raw event.Raw) event.Actor {
	return provider.resolve(ctx, raw, nil)
}

func (Provider) resolve(ctx context.Context, raw event.Raw, observedAt *time.Time) event.Actor {
	if ctx.Err() != nil || raw.PID <= 0 {
		return event.Actor{}
	}

	liveErr := alive(raw.PIDFD)
	if liveErr != nil {
		return event.Actor{UnavailableReason: liveErr.Error()}
	}

	process, err := procfs.NewProc(raw.PID)
	if err != nil {
		return event.Actor{UnavailableReason: "open process: " + err.Error()}
	}

	stat, err := process.Stat()
	if err != nil {
		return event.Actor{UnavailableReason: "read process stat: " + err.Error()}
	}

	if observedAt != nil {
		birth, birthErr := stat.StartTime()
		if birthErr != nil || birth >= float64(observedAt.Unix()-1) {
			return event.Actor{}
		}
	}

	actor := event.Actor{PID: raw.PID, StartTime: stat.Starttime, Known: true}

	enrich(process, &actor)

	after, err := process.Stat()
	if err != nil {
		return event.Actor{UnavailableReason: "recheck process stat: " + err.Error()}
	}

	if after.Starttime != stat.Starttime {
		return event.Actor{UnavailableReason: "process start time changed"}
	}

	liveErr = alive(raw.PIDFD)
	if liveErr != nil {
		return event.Actor{UnavailableReason: liveErr.Error()}
	}

	return actor
}

func alive(pidfd *os.File) error {
	if pidfd == nil {
		return nil
	}

	descriptor := pidfd.Fd()
	if descriptor > math.MaxInt32 {
		return fault.New("invalid pidfd")
	}

	fds := []unix.PollFd{{Fd: int32(descriptor), Events: unix.POLLIN, Revents: 0}}

	ready, err := unix.Poll(fds, 0)
	if err != nil {
		return fault.Wrap("poll pidfd", err)
	}

	if ready != 0 {
		return fault.New("pidfd reports exited process or invalid descriptor")
	}

	return nil
}

func enrich(process procfs.Proc, actor *event.Actor) {
	status, err := process.NewStatus()
	if err == nil && status.UIDs[0] <= math.MaxUint32 && status.UIDs[1] <= math.MaxUint32 {
		//nolint:gosec // Linux supplies bounded descriptors/UIDs; the adjacent checks or ABI define the range.
		actor.UID = uint32(status.UIDs[0])
		//nolint:gosec // Linux supplies bounded descriptors/UIDs; the adjacent checks or ABI define the range.
		actor.EUID = uint32(status.UIDs[1])
		actor.UserKnown = true
	}

	executable, executableErr := process.Executable()
	if executableErr == nil {
		actor.Executable = executable
	}

	command, commandErr := process.CmdLine()
	if commandErr == nil {
		actor.Command = strings.Join(command, " ")
	}

	enrichLogin(actor)
}

func enrichLogin(actor *event.Actor) {
	base := "/proc/" + strconv.Itoa(actor.PID)

	//nolint:gosec // The path is configured locally or built from a kernel PID; no remote path input is accepted.
	login, err := os.ReadFile(filepath.Join(base, "loginuid"))
	if err == nil {
		uid, parseErr := strconv.ParseUint(strings.TrimSpace(string(login)), 10, 32)
		if parseErr == nil && uid != math.MaxUint32 {
			actor.LoginUID = uint32(uid)
			actor.LoginKnown = true
		}
	}
}
