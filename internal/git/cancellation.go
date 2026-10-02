package git

import (
	"errors"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
)

// Bound pipe draining when a descendant has escaped the Git process group.
const commandWaitDelay = time.Second

func cancelGit(command *exec.Cmd) error {
	if command.Process == nil {
		return os.ErrProcessDone
	}

	groupErr := unix.Kill(-command.Process.Pid, unix.SIGKILL)
	// ESRCH refers to the group, not necessarily the direct child. Always try its handle too.
	processErr := command.Process.Kill()
	if groupErr == nil || processErr == nil {
		return nil
	}

	if errors.Is(groupErr, unix.ESRCH) {
		return fault.Wrap("cancel Git process", processErr)
	}

	return errors.Join(fault.Wrap("cancel Git process group", groupErr), fault.Wrap("cancel Git process", processErr))
}
