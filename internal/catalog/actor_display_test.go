package catalog_test

import (
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/integrity"
)

const (
	detectionFanotify = "fanotify"
	fieldNameSize     = "size"
	fieldNameDevice   = "device"
	actorProcess      = "apache (81): php-fpm modified files\n\nUser:\n  real: apache (81)\n  login: unknown\n" +
		"  effective: apache (81)\n\nProcess:\n  pid: 3785335\n  start_time: 17173045\n" +
		"  executable: \"/usr/lib64/php7.4/bin/php-fpm\"\n\nDetection:\n  backend: fanotify"
	actorConflict = "unknown: filesystem changes detected\n\nActor: unknown\n\nDetection:\n  backend: fanotify\n" +
		"Reason: conflicting actors"
	actorReconciliation = "unknown: filesystem changes detected\n\nActor: unknown\nDetection: reconciliation\n" +
		"Reason: overflow or retry"
)

func actorDetail(actor string) catalog.Detail {
	return catalog.Detail{Actor: actor, Kind: "", ChangeID: "", Path: nil, Fields: nil, Observed: 0, BackdatedBy: 0}
}

func TestActorLines(t *testing.T) {
	t.Parallel()

	for _, fixture := range []struct {
		name, actor, detection string
		want                   []string
	}{
		{
			name: "process", actor: actorProcess, detection: detectionFanotify,
			want: []string{
				"apache (81): php-fpm modified files", "User:", "  real: apache (81)",
				"  effective: apache (81)", "Process:", "  pid: 3785335", "  start_time: 17173045",
				"  executable: /usr/lib64/php7.4/bin/php-fpm",
			},
		},
		{
			name: "conflict", actor: actorConflict, detection: detectionFanotify,
			want: []string{"unknown: filesystem changes detected", "Reason: conflicting actors"},
		},
		{
			name: "reconciliation", actor: actorReconciliation, detection: "reconciliation",
			want: []string{"unknown: filesystem changes detected", "Reason: overflow or retry"},
		},
		{
			name: "git-trailer-and-evidence", detection: "fanotify,inotify",
			actor: "root (0): bash modified files\n\nProcess:\n  pid: 7\n" +
				"  start_time: 100 (2026-10-15 10:31:12 CEST)\n  executable: \"/usr/bin/bash\"\n\n" +
				"Attribution:\n  evidence: audit\n\nDetection:\n  backend: fanotify,inotify\n" +
				"FSLedger-Change-ID: ABC\n",
			want: []string{
				"root (0): bash modified files", "Process:", "  pid: 7", "  start_time: 2026-10-15 10:31:12 CEST",
				"  executable: /usr/bin/bash", "Attribution:", "  evidence: audit",
			},
		},
		{
			name: "known-login", actor: "  login: nataliachen (1000)\n  login: uid:12345", detection: "",
			want: []string{"  login: nataliachen (1000)", "  login: uid:12345"},
		},
		{name: "missing-login", actor: "  login: \r\n", detection: "", want: nil},
		{name: "empty", actor: "", detection: "", want: nil},
		{name: "plain", actor: "manual verification", detection: "", want: []string{"manual verification"}},
	} {
		detail := actorDetail(fixture.actor)
		if got := detail.ActorLines(); !reflect.DeepEqual(got, fixture.want) {
			t.Errorf("%s: ActorLines = %q, want %q", fixture.name, got, fixture.want)
		}

		if got := detail.Detection(); got != fixture.detection {
			t.Errorf("%s: Detection = %q, want %q", fixture.name, got, fixture.detection)
		}
	}
}

// Process data is attacker-controlled: it must never add lines that look like message structure.
func TestActorLinesEscapeProcessData(t *testing.T) {
	t.Parallel()

	actor := "evil\n\nProcess:\n  pid: 1\n  start_time: 2\n" +
		"  executable: \"/tmp/x\\nRuta: \\\"/etc/shadow\\\" / M\\r\\x1b[2J\\xff\"\n" +
		"Reason: bad\x1b[31m\rRuta: fake"

	lines := actorDetail(actor).ActorLines()
	if len(lines) != 6 {
		t.Fatalf("lines = %q", lines)
	}

	for _, line := range lines {
		if strings.ContainsAny(line, "\n\r\x1b") || strings.HasPrefix(line, "Ruta:") {
			t.Fatalf("unescaped process data %q in %q", line, lines)
		}
	}

	if want := `  executable: "/tmp/x\nRuta: \"/etc/shadow\" / M\r\x1b[2J\xff"`; lines[4] != want {
		t.Fatalf("executable = %q, want %q", lines[4], want)
	}

	if want := `Reason: bad\x1b[31m\rRuta: fake`; lines[5] != want {
		t.Fatalf("reason = %q, want %q", lines[5], want)
	}
}

func TestCommonDetection(t *testing.T) {
	t.Parallel()

	message := func(actors ...string) catalog.Message {
		details := make([]catalog.Detail, len(actors))
		for index, actor := range actors {
			details[index] = actorDetail(actor)
		}

		return catalog.Message{Details: details}
	}

	for _, fixture := range []struct {
		name, want string
		message    catalog.Message
	}{
		{"shared", detectionFanotify, message(actorProcess, actorConflict)},
		{"mixed", "", message(actorProcess, actorReconciliation)},
		{"missing", "", message(actorProcess, "manual verification")},
		{"none", "", message()},
	} {
		if got := fixture.message.CommonDetection(); got != fixture.want {
			t.Errorf("%s: CommonDetection = %q, want %q", fixture.name, got, fixture.want)
		}
	}
}

var displayStamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} \S+$`)

func displayField(name, before, after string) catalog.FieldChange {
	return catalog.FieldChange{Field: name, Before: before, After: after}
}

func TestDisplayFieldsHideCreationNoise(t *testing.T) {
	t.Parallel()

	created := displayDetail("added",
		displayField("type", `""`, `"regular"`),
		displayField(integrity.FieldHash, ":", "sha256:abc"),
		displayField(fieldNameSize, "0", "9475"),
		displayField(integrity.FieldMode, "0", "33212"),
		displayField("device", "0", "64514"),
		displayField("nlink", "0", "1"),
		displayField("mtime", "0", "1792230241202511342"),
		displayField("btime", "0 (known=false)", "1792230241202511342 (known=true)"),
		displayField("has_btime", "false", "true"),
		displayField("xattrs", "null ()", "null (available)"),
		displayField(displayACL, "null ()", "null (available)"),
		displayField("attributes_status", `""`, `"available"`),
	)

	shown := map[string]catalog.FieldChange{}
	for _, change := range created.DisplayFields() {
		shown[change.Field] = change
	}

	for _, hidden := range []string{"has_btime", "xattrs", displayACL, "attributes_status", "device", "nlink"} {
		if _, found := shown[hidden]; found {
			t.Errorf("creation shows %s", hidden)
		}
	}

	for name, after := range map[string]string{
		"type": "regular", integrity.FieldHash: "sha256:abc", fieldNameSize: "9.3 KiB",
		integrity.FieldMode: "-rw-rwxr-- (0x81bc)",
	} {
		if shown[name].Before != "—" || shown[name].After != after {
			t.Errorf("creation %s = %q -> %q, want — -> %q", name, shown[name].Before, shown[name].After, after)
		}
	}

	for _, name := range []string{"mtime", "btime"} {
		if !displayStamp.MatchString(shown[name].After) {
			t.Errorf("creation %s = %q", name, shown[name].After)
		}
	}
}

func displayNames(fields []catalog.FieldChange) []string {
	names := make([]string, len(fields))
	for index, field := range fields {
		names[index] = field.Field
	}

	return names
}

func TestDisplayFieldsShowLinksOnlyForSharedRegularFiles(t *testing.T) {
	t.Parallel()

	for _, fixture := range []struct {
		name   string
		detail catalog.Detail
		want   bool
	}{
		{
			"single-link",
			displayDetail("added", displayField("type", `""`, `"regular"`), displayField("nlink", "0", "1")),
			false,
		},
		{
			"hard-link",
			displayDetail("added", displayField("type", `""`, `"regular"`), displayField("nlink", "0", "2")),
			true,
		},
		{
			"directory",
			displayDetail("added", displayField("type", `""`, `"directory"`), displayField("nlink", "0", "5")),
			false,
		},
		{
			"deleted-link",
			displayDetail("deleted", displayField("type", `"regular"`, `""`), displayField("nlink", "3", "0")),
			true,
		},
	} {
		got := slices.Contains(displayNames(fixture.detail.DisplayFields()), "nlink")
		if got != fixture.want {
			t.Errorf("%s: nlink shown = %v, want %v", fixture.name, got, fixture.want)
		}
	}
}

func TestDisplayFieldsShowAttributeStatusOnlyWhenReadingFails(t *testing.T) {
	t.Parallel()

	denied := displayDetail("added",
		displayField("xattrs", "null ()", "null (denied)"),
		displayField(displayACL, "null ()", "null (denied)"),
		displayField("attributes_status", `""`, `"denied"`),
	)
	if got := denied.DisplayFields(); len(got) != 1 || got[0] != displayField("attributes_status", "—", "denied") {
		t.Errorf("failed read = %q", got)
	}

	readable := displayDetail("added", displayField("attributes_status", `""`, `"available"`))
	if got := readable.DisplayFields(); len(got) != 0 {
		t.Errorf("successful read shown: %q", got)
	}
}
