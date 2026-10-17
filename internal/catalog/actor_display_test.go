package catalog_test

import (
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/catalog"
)

const (
	detectionFanotify = "fanotify"
	fieldNameHash     = "hash"
	fieldNameMode     = "mode"
	fieldNameSize     = "size"
	actorProcess      = "apache (81): php-fpm modified files\n\nUser:\n  real: apache (81)\n  login: unknown\n" +
		"  effective: apache (81)\n\nProcess:\n  pid: 3785335\n  start_time: 17173045\n" +
		"  executable: \"/usr/lib64/php7.4/bin/php-fpm\"\n\nDetection:\n  backend: fanotify"
	actorConflict = "unknown: filesystem changes detected\n\nActor: unknown\n\nDetection:\n  backend: fanotify\n" +
		"Reason: conflicting actors"
	actorReconciliation = "unknown: filesystem changes detected\n\nActor: unknown\nDetection: reconciliation\n" +
		"Reason: overflow or retry"
)

func actorDetail(actor string) catalog.Detail {
	return catalog.Detail{Actor: actor, Kind: "", ChangeID: "", Path: nil, Fields: nil, Baseline: nil, Observed: 0}
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
				"apache (81): php-fpm modified files", "User:", "  real: apache (81)", "  login: unknown",
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

var (
	displayStamp   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} \S+$`)
	displayPrecise = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{9} \S+$`)
)

func displayField(name, before, after string) catalog.FieldChange {
	return catalog.FieldChange{Field: name, Before: before, After: after}
}

func TestDisplayFieldsHideCreationNoise(t *testing.T) {
	t.Parallel()

	created := displayDetail("added",
		displayField("type", `""`, `"regular"`),
		displayField(fieldNameHash, ":", "sha256:abc"),
		displayField(fieldNameSize, "0", "9475"),
		displayField(fieldNameMode, "0", "33212"),
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
		"type": "regular", fieldNameHash: "sha256:abc", fieldNameSize: "9.3 KiB", fieldNameMode: "-rw-rwxr-- (0x81bc)",
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

func TestDisplayFieldsMarkDeletedSide(t *testing.T) {
	t.Parallel()

	deleted := displayDetail("deleted",
		displayField(fieldNameSize, "182422", "0"), displayField(fieldNameHash, "sha256:ea5c", ":"))

	got := deleted.DisplayFields()
	if got[0].Before != "178.1 KiB" || got[0].After != "—" || got[1].After != "—" {
		t.Errorf("deletion = %q", got)
	}
}

func TestDisplayFieldsKeepSmallDifferencesVisible(t *testing.T) {
	t.Parallel()

	modified := displayDetail("modified",
		displayField("mtime", "1792230241202511342", "1792230241702511342"),
		displayField(fieldNameSize, "18374", "18380"),
		displayField(displayACL, "[] (available)", "[] (denied)"),
		displayField("btime", "1792230241202511342 (known=true)", "1792230241902511342 (known=true)"),
	)

	got := modified.DisplayFields()
	if len(got) != 4 || !displayPrecise.MatchString(got[0].Before) || got[0].Before == got[0].After {
		t.Fatalf("same-second mtime lost its difference: %q", got)
	}

	if got[1].Before != "17.9 KiB (18374 B)" || got[1].After != "17.9 KiB (18380 B)" {
		t.Errorf("small size change = %q -> %q", got[1].Before, got[1].After)
	}

	if want := displayField("attributes_status", "available", "denied"); got[2] != want {
		t.Errorf("failed ACL read = %q, want %q", got[2], want)
	}

	if !displayPrecise.MatchString(got[3].Before) || got[3].Before == got[3].After {
		t.Errorf("btime = %q -> %q", got[3].Before, got[3].After)
	}
}

// Attributes a filesystem does not provide are not evidence: they are hidden on any existing side.
func TestDisplayFieldsHideUnsupportedAttributes(t *testing.T) {
	t.Parallel()

	for _, fixture := range []struct {
		name   string
		detail catalog.Detail
	}{
		{"noatime-creation", displayDetail("added", displayField("atime", "0", "0"))},
		{"atime-lost", displayDetail("modified", displayField("atime", "1792230241202511342", "0"))},
		{"unknown-btime", displayDetail("modified",
			displayField("btime", "1792230241202511342 (known=true)", "0 (known=false)"))},
		{"btime-deleted-unknown", displayDetail("deleted", displayField("btime", "0 (known=false)", "0 (known=false)"))},
		{"unsupported-xattrs", displayDetail("added", displayField("xattrs", "null ()", "null (unsupported)"))},
		{"aide-acl", displayDetail("modified", displayField(displayACL, "null (not_recorded_by_aide)", "[] (available)"))},
	} {
		if got := fixture.detail.DisplayFields(); len(got) != 0 {
			t.Errorf("%s shown: %q", fixture.name, got)
		}
	}

	kept := displayDetail("modified", displayField("atime", "1792230241202511342", "1792230300202511342"))
	if got := kept.DisplayFields(); len(got) != 1 {
		t.Errorf("maintained atime hidden: %q", got)
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
		{"compared-change", displayDetail("modified", displayField("nlink", "1", "2")), true},
	} {
		got := slices.Contains(displayNames(fixture.detail.DisplayFields()), "nlink")
		if got != fixture.want {
			t.Errorf("%s: nlink shown = %v, want %v", fixture.name, got, fixture.want)
		}
	}
}

func TestDisplayFieldsShowAttributeStatusOnlyWhenReadingFails(t *testing.T) {
	t.Parallel()

	unsupported := displayDetail("added",
		displayField("xattrs", "null ()", "null (unsupported)"),
		displayField(displayACL, "null ()", "null (unsupported)"),
		displayField("attributes_status", `""`, `"unsupported"`),
	)
	if got := unsupported.DisplayFields(); len(got) != 0 {
		t.Errorf("missing filesystem support shown as a failed read: %q", got)
	}

	denied := displayDetail("added",
		displayField("xattrs", "null ()", "null (denied)"),
		displayField(displayACL, "null ()", "null (denied)"),
		displayField("attributes_status", `""`, `"denied"`),
	)
	if got := denied.DisplayFields(); len(got) != 1 || got[0] != displayField("attributes_status", "—", "denied") {
		t.Errorf("failed read = %q", got)
	}

	recovered := displayDetail("modified", displayField(displayACL, "[] (denied)", "[] (available)"))
	if got := recovered.DisplayFields(); len(got) != 1 || got[0].Before != "denied" {
		t.Errorf("recovered read = %q", got)
	}

	readable := displayDetail("added", displayField("attributes_status", `""`, `"available"`))
	if got := readable.DisplayFields(); len(got) != 0 {
		t.Errorf("successful read shown: %q", got)
	}
}

func TestDisplayFieldsKeepComparedDevice(t *testing.T) {
	t.Parallel()

	moved := displayDetail("modified", displayField("device", "64514", "2049"))
	if got := moved.DisplayFields(); len(got) != 1 || got[0] != displayField("device", "64514", "2049") {
		t.Errorf("compared device change = %q", got)
	}
}

func TestBackdatedBy(t *testing.T) {
	t.Parallel()

	const (
		btime     = "1792230241202511342 (known=true)"
		threeYear = 1095 * 24 * time.Hour
	)

	born := time.Unix(0, 1792230241202511342)
	stamp := func(at time.Time) string { return strconv.FormatInt(at.UnixNano(), 10) }

	for _, fixture := range []struct {
		name   string
		detail catalog.Detail
		want   time.Duration
	}{
		{
			"backdated-creation", displayDetail("added",
				displayField("mtime", "0", stamp(born.Add(-threeYear))), displayField("btime", "0 (known=false)", btime)),
			threeYear,
		},
		{"fresh-creation", displayDetail("added",
			displayField("mtime", "0", stamp(born)), displayField("btime", "0 (known=false)", btime)), 0},
		{
			"short-copy-delay", displayDetail("added",
				displayField("mtime", "0", stamp(born.Add(-30*time.Minute))), displayField("btime", "0 (known=false)", btime)),
			0,
		},
		{
			"backdated-deletion", displayDetail("deleted",
				displayField("mtime", stamp(born.Add(-threeYear)), "0"), displayField("btime", btime, "0 (known=false)")),
			threeYear,
		},
		{"unknown-btime", displayDetail("added",
			displayField("mtime", "0", stamp(born.Add(-threeYear))),
			displayField("btime", "0 (known=false)", "0 (known=false)")), 0},
		{"no-times", displayDetail("modified", displayField(fieldNameHash, "sha256:a", "sha256:b")), 0},
	} {
		if got := fixture.detail.BackdatedBy(); got != fixture.want {
			t.Errorf("%s: BackdatedBy = %v, want %v", fixture.name, got, fixture.want)
		}
	}
}
