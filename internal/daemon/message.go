package daemon

import (
	"fmt"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	procattr "github.com/inode64/fsledger/internal/attribution/proc"
	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/humanize"
)

const unknownActorMessage = event.Unknown + ": " + event.ActorChangesDetected + "\n\n" + event.ActorUnknownLine

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
			`%s: `+event.ActorChangesDetected+`

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

	// The name is process-controlled: its control characters and invalid bytes, which could add lines to the
	// commit message, become U+FFFD. Escaping them instead would be escaped again where notices display the
	// line; the executable line keeps the exact name.
	executable := strings.Map(func(char rune) rune {
		if unicode.IsPrint(char) {
			return char
		}

		return utf8.RuneError
	}, filepath.Base(actor.Executable))
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
  `+event.ActorStartTime+` %d%s
  `+event.ActorExecutable+` %q`,
		observed, executable, realUser, login, effective, actor.PID, actor.StartTime,
		processStart(actor.StartTime), actor.Executable)
}

func reconciliationMessage(reason string) string {
	return unknownActorMessage +
		"\n" + event.ActorDetection + " " + event.Reconciliation +
		"\n" + event.ActorReason + " " + reason
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

	message += "\n\n" + event.ActorDetection + "\n  " + event.ActorBackend + " " + strings.Join(detection, ",")
	if group.Reason != "" {
		message += "\n" + event.ActorReason + " " + group.Reason
	}

	return message
}
