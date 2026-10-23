package daemon

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/event"
)

type actorDisplayFixture struct {
	name, message, detection string
	contains, absent         []string
}

func actorDisplayGroup(actor event.Actor, reason string, backends ...string) event.Group {
	paths := make(map[string]event.Raw, len(backends))
	for index, backend := range backends {
		paths[string(rune('a'+index))] = event.Raw{Backend: backend}
	}

	return event.Group{Actor: actor, Paths: paths, Reason: reason}
}

func actorDisplayFixtures() []actorDisplayFixture {
	process := event.Actor{
		Known: true, UserKnown: true, PID: 4242, StartTime: 12345, Executable: "/usr/bin/bash", Evidence: "audit",
	}

	return []actorDisplayFixture{
		{
			name:      "process",
			message:   groupMessage(actorDisplayGroup(process, "", "fanotify", "inotify"), false),
			detection: "fanotify,inotify",
			contains:  []string{"Process:", "  pid: 4242", "  executable: /usr/bin/bash", "  evidence: audit"},
			absent:    []string{"Detection:", "  backend: fanotify,inotify"},
		},
		{
			name: "grouped-processes",
			message: groupMessage(
				actorDisplayGroup(event.Actor{Known: true, UserKnown: true}, "", "fanotify"),
				false,
			),
			detection: "fanotify",
			contains:  []string{"Process: unknown (grouped across processes)"},
			absent:    nil,
		},
		{
			name:      "conflict",
			message:   groupMessage(actorDisplayGroup(event.Actor{}, "conflicting actors", "fanotify"), false),
			detection: "fanotify",
			contains:  []string{"unknown: filesystem changes detected", "Reason: conflicting actors"},
			absent:    []string{"Actor: unknown"},
		},
		{
			name:      "reconciliation",
			message:   reconciliationMessage("overflow or retry"),
			detection: event.Reconciliation,
			contains:  []string{"Reason: overflow or retry"},
			absent:    []string{"Detection: " + event.Reconciliation},
		},
	}
}

// The catalog derives display lines from the commit text written here; both sides must agree.
func TestActorMessagesRenderAsDisplayLines(t *testing.T) {
	t.Parallel()

	for _, fixture := range actorDisplayFixtures() {
		detail := catalog.Detail{
			Actor: fixture.message, ChangeID: "", Kind: "", Path: nil, Fields: nil, Observed: 0,
			BackdatedBy: 0,
		}
		lines := detail.ActorLines()

		for _, want := range fixture.contains {
			if !slices.Contains(lines, want) {
				t.Errorf("%s: missing %q in %q", fixture.name, want, lines)
			}
		}

		for _, unwanted := range fixture.absent {
			if slices.Contains(lines, unwanted) {
				t.Errorf("%s: unexpected %q in %q", fixture.name, unwanted, lines)
			}
		}

		if got := detail.Detection(); got != fixture.detection {
			t.Errorf("%s: Detection = %q, want %q", fixture.name, got, fixture.detection)
		}
	}
}

func TestActorMessageKeepsRawStartTime(t *testing.T) {
	t.Parallel()

	if processStart(0) != "" {
		t.Fatal("an unknown start time must not be annotated")
	}

	message := actorMessage(event.Actor{Known: true, PID: 1, StartTime: 17173045, Executable: "/bin/sh"}, false)

	annotated := regexp.MustCompile(`(?m)^  start_time: 17173045( \(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} \S+\))?$`)
	if !annotated.MatchString(message) || !strings.Contains(message, "executable: \"/bin/sh\"") {
		t.Fatalf("raw start time or executable lost: %q", message)
	}
}

// A process names itself: its executable must not add lines, such as a forged trailer, to the subject. Notices
// display the subject escaped once, as any line, never the name escaped twice.
func TestActorMessageEscapesExecutableName(t *testing.T) {
	t.Parallel()

	names := map[string]string{
		"/tmp/x\nFSLedger-Change-ID: forged": "x\uFFFDFSLedger-Change-ID: forged",
		"/tmp/a\\b":                          "a\\\\b",
		"/tmp/\xff":                          "\uFFFD",
	}
	for executable, displayed := range names {
		message := actorMessage(event.Actor{Known: true, PID: 1, StartTime: 1, Executable: executable}, false)

		subject, _, _ := strings.Cut(message, "\n")
		if !strings.HasSuffix(subject, " modified files") || strings.Count(message, "\nFSLedger") != 0 {
			t.Fatalf("executable name broke the message: %q", message)
		}

		lines := catalog.Detail{
			Actor: message, ChangeID: "", Kind: "", Path: nil, Fields: nil, Observed: 0, BackdatedBy: 0,
		}.ActorLines()
		if len(lines) == 0 || !strings.HasSuffix(lines[0], ": "+displayed+" modified files") {
			t.Fatalf("notice shows %q for %q", lines, executable)
		}
	}
}
