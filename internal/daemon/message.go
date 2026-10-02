package daemon

import (
	"fmt"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/inode64/fsledger/internal/event"
)

const unknownActorMessage = "unknown: filesystem changes detected\n\nActor: unknown"

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
  start_time: %d
  executable: %q`,
		observed, executable, realUser, login, effective, actor.PID, actor.StartTime, actor.Executable)
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
