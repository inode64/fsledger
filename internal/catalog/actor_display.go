package catalog

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	gitrepo "github.com/inode64/fsledger/internal/git"
	"github.com/inode64/fsledger/internal/humanize"
)

// The daemon stores the actor as the rendered commit message (internal/daemon/message.go).
// These views derive readable lines from that text, so deliveries queued by older versions
// render the same way without migrating stored evidence.
const (
	detectionHeader     = "Detection:"
	detectionBackend    = "backend:"
	unknownActorSummary = "unknown:"
	unknownActorLine    = "Actor: unknown"
	executableKey       = "executable:"
)

// startTimePattern matches "start_time: <ticks> (<rendered time>)" written by newer daemons.
var startTimePattern = regexp.MustCompile(`^(\s*start_time: )\d+ \((.+)\)$`)

// ActorLines returns the actor evidence as display lines: blank lines, the Git change trailer and
// the detection method are dropped, the executable is unquoted when printable and every line is
// escaped so process-controlled text cannot inject lines into a message.
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
		case strings.HasPrefix(trimmed, gitrepo.ChangeIDTrailer):
			continue
		case unknown && trimmed == unknownActorLine:
			continue
		case trimmed == detectionHeader:
			for index+1 < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[index+1]), detectionBackend) {
				index++
			}

			continue
		case strings.HasPrefix(trimmed, detectionHeader+" "):
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

	indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
	if value, found := strings.CutPrefix(strings.TrimLeft(line, " "), executableKey+" "); found {
		unquoted, err := strconv.Unquote(value)
		if err == nil {
			if utf8.ValidString(unquoted) && humanize.Printable(unquoted) == unquoted {
				return indent + executableKey + " " + unquoted
			}

			// Keep the quoted form so escapes are not escaped twice; Quote emits no control bytes.
			return indent + executableKey + " " + strconv.Quote(unquoted)
		}
	}

	return humanize.Printable(line)
}

// Detection names how the change was observed (fanotify, inotify, reconciliation...), or "".
func (detail Detail) Detection() string {
	lines := strings.Split(detail.Actor, "\n")

	for index, line := range lines {
		trimmed := strings.TrimSpace(line)

		if value, found := strings.CutPrefix(trimmed, detectionHeader+" "); found {
			return humanize.Printable(strings.TrimSpace(value))
		}

		if trimmed == detectionHeader && index+1 < len(lines) {
			value, found := strings.CutPrefix(strings.TrimSpace(lines[index+1]), detectionBackend)
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
