package daemon

import (
	"fmt"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	procattr "github.com/inode64/fsledger/internal/attribution/proc"
	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/humanize"
)

const unknownActorMessage = "unknown: filesystem changes detected\n\nActor: unknown"

// processStart annotates the raw start time (the identity evidence, kept as is) with wall-clock time.
func processStart(ticks uint64) string {
	started, ok := procattr.StartTime(ticks)
	if !ok {
		return ""
	}

	return " (" + humanize.Time(started.UnixNano()) + ")"
}

func userName(uid uint32) string {
	identifier := strconv.FormatUint(uint64(uid), 10)

	account, err := user.LookupId(identifier)
	if err != nil {
		return "uid:" + identifier
	}

	return account.Username + " (" + identifier + ")"
}

func actorMessage(actor event.Actor, preferLogin bool) string {
	if !actor.Known {
		return unknownActorMessage
	}

	realUser, effective, login := event.Unknown, event.Unknown, event.Unknown
	if actor.UserKnown {
		realUser = userName(actor.UID)
		effective = userName(actor.EUID)
	}

	if actor.LoginKnown {
		login = userName(actor.LoginUID)
	}

	observed := realUser
	if preferLogin && actor.LoginKnown {
		observed = login
	}

	if actor.PID == 0 {
		return fmt.Sprintf(
			`%s: filesystem changes detected

User:
  real: %s
  login: %s
  effective: %s

Process: unknown (grouped across processes)`,
			observed,
			realUser,
			login,
			effective,
		)
	}

	executable := filepath.Base(actor.Executable)
	if actor.Executable == "" {
		executable = "process"
	}

	return fmt.Sprintf(`%s: %s modified files

User:
  real: %s
  login: %s
  effective: %s

Process:
  pid: %d
  start_time: %d%s
  executable: %q`,
		observed, executable, realUser, login, effective, actor.PID, actor.StartTime,
		processStart(actor.StartTime), actor.Executable)
}

func reconciliationMessage(reason string) string {
	return unknownActorMessage + "\nDetection: " + event.Reconciliation + "\nReason: " + reason
}

func groupMessage(group event.Group, preferLogin bool) string {
	backends := make(map[string]bool)
	for _, raw := range group.Paths {
		backends[raw.Backend] = true
	}

	detection := make([]string, 0, len(backends))
	for backend := range backends {
		detection = append(detection, backend)
	}

	sort.Strings(detection)

	message := actorMessage(group.Actor, preferLogin)
	if group.Actor.Evidence != "" {
		message += "\n\nAttribution:\n  evidence: " + group.Actor.Evidence
	}

	message += "\n\nDetection:\n  backend: " + strings.Join(detection, ",")
	if group.Reason != "" {
		message += "\nReason: " + group.Reason
	}

	return message
}
