package catalog

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/inode64/fsledger/internal/event"
	gitrepo "github.com/inode64/fsledger/internal/git"
	"github.com/inode64/fsledger/internal/humanize"
)

// The daemon stores the actor as the rendered commit message with the labels of internal/event.
// These views derive readable lines from that text, so deliveries queued by older versions
// render the same way without migrating stored evidence.
const unknownActorSummary = event.Unknown + ":"

// startTimePattern matches "start_time: <ticks> (<rendered time>)" written by newer daemons.
var startTimePattern = regexp.MustCompile(`^(\s*` + regexp.QuoteMeta(event.ActorStartTime) + ` )\d+ \((.+)\)$`)

// ActorLines returns the actor evidence as display lines: blank lines, the Git change trailer and
// the detection method are dropped, as is login when no identity was recorded. The executable is
// unquoted when printable and every line is escaped so process-controlled text cannot inject lines.
func (detail Detail) ActorLines() []string {
	lines := strings.Split(detail.Actor, "\n")
	unknown := strings.HasPrefix(lines[0], unknownActorSummary)

	out := make([]string, 0, len(lines))

	for index := 0; index < len(lines); index++ {
		line := strings.TrimRight(lines[index], " \t\r")
		trimmed := strings.TrimSpace(line)

		switch {
		case trimmed == "":
			continue
		case trimmed == "login:" || trimmed == "login: "+event.Unknown:
			continue
		case strings.HasPrefix(trimmed, gitrepo.ChangeIDTrailer):
			continue
		case unknown && trimmed == event.ActorUnknownLine:
			continue
		case trimmed == event.ActorDetection:
			for index+1 < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[index+1]), event.ActorBackend) {
				index++
			}

			continue
		case strings.HasPrefix(trimmed, event.ActorDetection+" "):
			continue
		}

		out = append(out, displayActorLine(line))
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

func displayActorLine(line string) string {
	if match := startTimePattern.FindStringSubmatch(line); match != nil {
		return humanize.Printable(match[1] + match[2])
	}

	content := strings.TrimLeft(line, " ")
	indent := line[:len(line)-len(content)]

	if value, found := strings.CutPrefix(content, event.ActorExecutable+" "); found {
		unquoted, err := strconv.Unquote(value)
		if err == nil {
			if utf8.ValidString(unquoted) && humanize.Printable(unquoted) == unquoted {
				return indent + event.ActorExecutable + " " + unquoted
			}

			// Keep the quoted form so escapes are not escaped twice; Quote emits no control bytes.
			return indent + event.ActorExecutable + " " + strconv.Quote(unquoted)
		}
	}

	return humanize.Printable(line)
}

// Detection names how the change was observed (fanotify, inotify, reconciliation...), or "".
func (detail Detail) Detection() string {
	lines := strings.Split(detail.Actor, "\n")

	for index, line := range lines {
		trimmed := strings.TrimSpace(line)

		if value, found := strings.CutPrefix(trimmed, event.ActorDetection+" "); found {
			return humanize.Printable(strings.TrimSpace(value))
		}

		if trimmed == event.ActorDetection && index+1 < len(lines) {
			value, found := strings.CutPrefix(strings.TrimSpace(lines[index+1]), event.ActorBackend)
			if found {
				return humanize.Printable(strings.TrimSpace(value))
			}
		}
	}

	return ""
}

// CommonDetection returns the detection method shared by every detail, so a message can state it
// once. It is empty when details disagree or none records one; templates then show it per path.
func (message Message) CommonDetection() string {
	common := ""

	for index, detail := range message.Details {
		detection := detail.Detection()
		if detection == "" || (index > 0 && detection != common) {
			return ""
		}

		common = detection
	}

	return common
}

// DisplayReason returns Reason with control characters and invalid UTF-8 escaped. Reasons quote
// filesystem text such as mount points, source globs and path errors, so a crafted name cannot
// inject lines into a notice. Reason keeps the stored text.
func (message Message) DisplayReason() string { return humanize.Printable(message.Reason) }

// detailedPaths is the largest summary whose paths are listed with their fields. Larger summaries,
// such as plugin or core updates, list every path grouped by actor, so one email carries far more.
const detailedPaths = 250

// Detailed reports whether every path of the message is listed with its fields.
func (message Message) Detailed() bool { return message.Count <= detailedPaths }

// DetailLimit is the largest number of paths a message lists with their fields.
func (Message) DetailLimit() int { return detailedPaths }
