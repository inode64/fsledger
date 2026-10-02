package audit

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/elastic/go-libaudit/v2/auparse"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/resource"
)

func fixture(t *testing.T, observedAt time.Time, serial, uid int, path string) []string {
	t.Helper()

	var stat unix.Stat_t

	statErr := unix.Lstat(path, &stat)
	if statErr != nil {
		t.Fatal(statErr)
	}

	header := fmt.Sprintf(
		"msg=audit(%d.%03d:%d): ",
		observedAt.Unix(),
		observedAt.Nanosecond()/int(time.Millisecond),
		serial,
	)

	return []string{
		"type=SYSCALL " + header + fmt.Sprintf(
			`arch=c000003e syscall=257 success=yes exit=3 a0=ffffff9c a1=0 a2=241 a3=1a4 `+
				`items=1 ppid=99 pid=%d auid=%d uid=%d gid=1000 euid=0 suid=0 fsuid=0 `+
				`egid=0 sgid=0 fsgid=0 tty=pts0 ses=7 comm="sed" exe="/usr/bin/sed" key="fsledger"`,
			2000000000+uid,
			uid,
			uid,
		),
		"type=CWD " + header + `cwd="/tmp"`,
		"type=PATH " + header + fmt.Sprintf(
			`item=0 name=%q inode=%d dev=%x:%x mode=0100600 ouid=0 ogid=0 rdev=00:00 nametype=NORMAL`,
			path, stat.Ino, unix.Major(stat.Dev), unix.Minor(stat.Dev),
		),
		"type=PROCTITLE " + header + "proctitle=736564002d69",
	}
}

func processor(t *testing.T) *Provider {
	t.Helper()

	provider, err := newProvider(map[string]func(string) bool{"fsledger": filepath.IsAbs},
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.Close(provider) })

	return provider
}

func pushLines(t *testing.T, provider *Provider, lines []string) {
	t.Helper()

	for _, line := range lines {
		message, err := auparse.ParseLogLine(line)
		if err != nil {
			t.Fatal(err)
		}

		provider.reassembler.PushMessage(message)
	}
}

func TestRecordedActorAfterProcessExit(t *testing.T) {
	t.Parallel()
	provider := processor(t)
	path := auditFixture(t)
	observedAt := time.Now().Add(-2 * matchWindow)
	pushLines(t, provider, fixture(t, observedAt, 10, 1001, path))

	actor := provider.Resolve(
		t.Context(),
		event.Raw{Path: path, Time: observedAt, Operation: event.Write, Backend: event.Inotify},
		"fsledger",
	)
	if !actor.Known || actor.UID != 1001 || actor.EUID != 0 || actor.LoginUID != 1001 || actor.Evidence == "" {
		t.Fatalf("recorded actor missing: %+v; observations=%+v", actor, provider.observations)
	}

	if actor.StartTime != 0 {
		t.Fatal("invented process start time")
	}
}

func TestInterleavedAuditRecords(t *testing.T) {
	t.Parallel()
	provider := processor(t)
	path := auditFixture(t)
	observedAt := time.Now().Add(-2 * matchWindow)

	secondPath := auditFixture(t)

	first, second := fixture(t, observedAt, 20, 1001, path), fixture(t, observedAt, 21, 1002, secondPath)
	for index := range first {
		pushLines(t, provider, []string{first[index], second[index]})
	}

	for path, uid := range map[string]uint32{path: 1001, secondPath: 1002} {
		actor := provider.Resolve(
			t.Context(),
			event.Raw{Path: path, Time: observedAt, Operation: event.Write, Backend: event.Inotify},
			"fsledger",
		)
		if !actor.Known || actor.UID != uid {
			t.Fatalf("crossed Audit actors for %s: %+v", path, actor)
		}
	}
}

//nolint:gocognit // Each case deliberately alters a distinct safety condition in the same replay fixture.
func TestAuditRejectsUnsafeMatches(t *testing.T) {
	t.Parallel()

	for _, scenario := range []string{
		"missing-path", "failed", "read-only", "wrong-key", "late",
		"ambiguous", "pid-mismatch", "lost", "excluded", "replaced-inode",
	} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			provider := processor(t)
			path := auditFixture(t)
			observedAt := time.Now().Add(-2 * matchWindow)
			lines := fixture(t, observedAt, 30, 1001, path)
			raw := event.Raw{Path: path, Time: observedAt, Operation: event.Write, Backend: event.Inotify}

			switch scenario {
			case "missing-path":
				lines = append(lines[:2], lines[3:]...)
			case "failed":
				lines[0] = strings.ReplaceAll(lines[0], "success=yes", "success=no")
			case "read-only":
				lines[0] = strings.ReplaceAll(lines[0], "a2=241", "a2=0")
			case "wrong-key":
				lines[0] = strings.ReplaceAll(lines[0], `key="fsledger"`, `key="other"`)
			case "late":
				lines = fixture(t, observedAt.Add(-retention), 30, 1001, path)
			case "pid-mismatch":
				raw.PID = 42
			case "replaced-inode":
				renameErr := os.Rename(path, path+".old")
				if renameErr != nil {
					t.Fatal(renameErr)
				}

				writeErr := os.WriteFile(path, []byte("different inode"), 0o600)
				if writeErr != nil {
					t.Fatal(writeErr)
				}
			case "excluded":
				provider.scopes["fsledger"] = func(string) bool { return false }
			}

			pushLines(t, provider, lines)

			if scenario == "ambiguous" {
				pushLines(t, provider, fixture(t, observedAt, 31, 1002, path))
			}

			if scenario == "lost" {
				provider.EventsLost(1)
			}

			if actor := provider.Resolve(t.Context(), raw, "fsledger"); actor.Known {
				t.Fatalf("unsafe attribution: %+v", actor)
			}
		})
	}
}

func auditFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")

	err := os.WriteFile(path, []byte("fixture"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

func TestUnverifiedPIDDoesNotProveActorIdentity(t *testing.T) {
	t.Parallel()

	first := event.Actor{Known: true, PID: 123, UID: 1000}

	second := first
	if sameActor(first, second) {
		t.Fatal("empty evidence treated a reusable PID as verified identity")
	}

	first.Evidence, second.Evidence = "audit:1", "audit:2"
	if sameActor(first, second) {
		t.Fatal("distinct observations merged without a verified process lifetime")
	}

	first.StartTime, second.StartTime = 42, 42
	if !sameActor(first, second) {
		t.Fatal("same verified process split across syscalls")
	}
}

func TestSharedProviderIsolatesKeysAndPaths(t *testing.T) {
	t.Parallel()

	const firstKey, secondKey = "first", "second"

	provider := processor(t)
	first, second := auditFixture(t), auditFixture(t)
	provider.scopes = map[string]func(string) bool{
		firstKey:  func(path string) bool { return path == first },
		secondKey: func(path string) bool { return path == second },
		"other":   filepath.IsAbs,
	}

	observedAt := time.Now().Add(-2 * matchWindow)
	for index, path := range []string{first, second} {
		lines := fixture(t, observedAt, 300+index, 1001+index, path)
		key := []string{firstKey, secondKey}[index]
		lines[0] = strings.ReplaceAll(lines[0], `key="fsledger"`, `key="`+key+`"`)
		pushLines(t, provider, lines)
	}

	if len(provider.observations) != 2 {
		t.Fatal("events lost or reconstructed more than once", len(provider.observations))
	}

	for index, path := range []string{first, second} {
		raw := event.Raw{Path: path, Time: observedAt, Operation: event.Write, Backend: event.Inotify}
		for _, key := range []string{firstKey, secondKey, "other", "missing"} {
			actor := provider.Resolve(t.Context(), raw, key)

			expected := key == []string{firstKey, secondKey}[index]
			if actor.Known != expected {
				t.Fatalf("cross-scope attribution: path=%s key=%s actor=%+v", path, key, actor)
			}
		}
	}
}
