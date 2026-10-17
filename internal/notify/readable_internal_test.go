package notify

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
)

const (
	readableHash  = "hash"
	readableMode  = "mode"
	readableMtime = "mtime"
	readablePHP   = "apache (81): php-fpm modified files\n\nUser:\n  real: apache (81)\n  login: unknown\n" +
		"  effective: apache (81)\n\nProcess:\n  pid: 3785335\n  start_time: 17173045\n" +
		"  executable: \"/usr/lib64/php7.4/bin/php-fpm\"\n\nDetection:\n  backend: fanotify"
	readableConflict = "unknown: filesystem changes detected\n\nActor: unknown\n\nDetection:\n  backend: fanotify\n" +
		"Reason: conflicting actors"
	readableFTP = "apache (81): proftpd modified files\n\nUser:\n  real: apache (81)\n  login: unknown\n" +
		"  effective: apache (81)\n\nProcess:\n  pid: 2072968\n  start_time: 1299838581\n" +
		"  executable: \"/usr/bin/proftpd\"\n\nDetection:\n  backend: fanotify"
	readableReconciliation = "unknown: filesystem changes detected\n\nActor: unknown\nDetection: reconciliation\n" +
		"Reason: overflow or retry"
)

func readableDetail(path, kind, actor string, fields ...catalog.FieldChange) catalog.Detail {
	return catalog.Detail{
		Path: []byte(path), Kind: kind, Actor: actor, ChangeID: "PBHEY4LZLTYDYX3O7OYWIBYK3H",
		Observed: 1792138666102146517, Fields: fields, Baseline: nil,
	}
}

func readableMessage(details ...catalog.Detail) catalog.Message {
	paths := make([]string, len(details))
	for index := range details {
		paths[index] = string(details[index].Path)
	}

	return catalog.Message{
		Repository: "promopublic.es", Host: "kvm2.promopublic.es", Hostname: "kvm2.promopublic.es",
		Event: config.NotificationChange, Paths: paths, Details: details, Count: len(details),
	}
}

func renderDefault(t *testing.T, message catalog.Message) string {
	t.Helper()

	rendered, err := New(nil).render(config.Notifier{}, message)
	if err != nil {
		t.Fatal(err)
	}

	return rendered.body
}

// The examples come from real alerts that were hard to read.
func TestDefaultBodyIsReadable(t *testing.T) {
	t.Parallel()

	hash := func(before, after string) catalog.FieldChange {
		return catalog.FieldChange{Field: readableHash, Before: "sha256:" + before, After: "sha256:" + after}
	}
	body := renderDefault(t, readableMessage(
		readableDetail("/var/www/promopublic.es/htdocs/wp-content/themes/chenplate/footer.php", "modified",
			readablePHP, hash("55f2", "7db5")),
		readableDetail("/var/www/promopublic.es/htdocs/wp-content/themes/chenplate/footer.php", "modified",
			readableConflict, hash("7db5", "650f")),
		readableDetail("/var/www/skillsonlinexams.com/htdocs/comunes/canonical.php", "added", readableFTP,
			catalog.FieldChange{Field: "type", Before: `""`, After: `"regular"`},
			catalog.FieldChange{Field: "size", Before: "0", After: "9475"},
			catalog.FieldChange{Field: readableMode, Before: "0", After: "33188"},
			catalog.FieldChange{Field: readableMtime, Before: "0", After: "1792230241202511342"},
			catalog.FieldChange{Field: "xattrs", Before: "null ()", After: "null (available)"},
			catalog.FieldChange{Field: "acl", Before: "null ()", After: "null (available)"},
			catalog.FieldChange{Field: "attributes_status", Before: `""`, After: `"available"`}),
	))
	t.Log("\n" + body)

	for _, unwanted := range []string{`\n`, `\"`, "backend:", "attributes_status", "xattrs", "acl:", "T08:17"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("body still contains %q", unwanted)
		}
	}

	for _, wanted := range []string{
		"Detección: fanotify\n",
		"Actor:\n    apache (81): php-fpm modified files\n    User:\n      real: apache (81)\n",
		"      executable: /usr/lib64/php7.4/bin/php-fpm\n",
		"Actor:\n    unknown: filesystem changes detected\n    Reason: conflicting actors\n",
		"size: — -> 9.3 KiB\n", "mode: — -> -rw-r--r-- (0x81a4)\n", "type: — -> regular\n",
	} {
		if !strings.Contains(body, wanted) {
			t.Errorf("body lacks %q", wanted)
		}
	}

	if strings.Count(body, "Detección:") != 1 {
		t.Error("a shared detection method must be stated once")
	}

	observed := regexp.MustCompile(`(?m)^Observado: \d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} \S+$`)
	if len(observed.FindAllString(body, -1)) != 3 {
		t.Error("observation times are not human readable")
	}
}

func TestDefaultBodyShowsMixedDetectionPerPath(t *testing.T) {
	t.Parallel()

	body := renderDefault(t, readableMessage(
		readableDetail("/srv/a", "modified", readablePHP),
		readableDetail("/srv/b", "added", readableReconciliation),
	))

	header, _, found := strings.Cut(body, "Elementos cambiados")
	if !found || strings.Contains(header, "Detección") ||
		!strings.Contains(body, "Detección: fanotify\nActor:") ||
		!strings.Contains(body, "Detección: reconciliation\nActor:") {
		t.Fatal("mixed detection must be shown per path\n" + body)
	}
}

func readableDetails(count int) []catalog.Detail {
	details := make([]catalog.Detail, count)
	for index := range details {
		actor, kind := readablePHP, "deleted"
		if index >= count/2 {
			actor, kind = readableReconciliation, "added"
		}

		details[index] = readableDetail(
			fmt.Sprintf("/var/www/site/htdocs/wp-content/plugins/updraftplus/vendor/guzzle/src/file-%05d.php", index),
			kind, actor,
			catalog.FieldChange{Field: readableHash, Before: "sha256:ea5ca52f2881c1015d17dfd85d3196b9", After: ":"},
			catalog.FieldChange{Field: "size", Before: "182422", After: "0"},
			catalog.FieldChange{Field: readableMtime, Before: "1790027051609144111", After: "0"},
			catalog.FieldChange{Field: "ctime", Before: "1790027051609144111", After: "0"},
		)
		details[index].ChangeID = fmt.Sprintf("CHANGE%04d", index/64)
	}

	return details
}

func TestDefaultBodyCompactsAbove250Paths(t *testing.T) {
	t.Parallel()

	full := renderDefault(t, readableMessage(readableDetails(250)...))
	if !strings.Contains(full, "Detalle de cambios") || strings.Contains(full, "agrupados por actor") {
		t.Fatal("250 paths must keep per-path details")
	}

	compact := renderDefault(t, readableMessage(readableDetails(251)...))
	if strings.Contains(compact, "Detalle de cambios") || !strings.Contains(compact, "agrupados por actor") ||
		strings.Count(compact, `"/var/www/site/`) != 251 {
		t.Fatal("251 paths must list every path grouped by actor")
	}
}

// A plugin update used to arrive as one email every ~470 paths.
func TestLargeUpdateFitsTwoEmails(t *testing.T) {
	t.Parallel()

	details := readableDetails(3500)
	deliveries := make([]catalog.Delivery, 0, len(details)/64+1)

	for start := 0; start < len(details); start += 64 {
		message := readableMessage(details[start:min(start+64, len(details))]...)
		message.ChangeID = details[start].ChangeID
		deliveries = append(deliveries, catalog.Delivery{
			ID: int64(len(deliveries) + 1), Message: message, Destination: "email-ops", Version: "",
			LastError: "", Attempts: 0, NextAttempt: 0,
		})
	}

	sender, emails := New(nil), 0
	notifier := config.Notifier{Type: config.NotifierEmail, DSN: "", URL: "", From: "", Template: "", To: nil}

	for len(deliveries) > 0 {
		chunk, _ := sender.digestChunk(notifier, catalog.MergeMessages(deliveries)[0])
		deliveries = deliveries[len(chunk.Deliveries):]
		emails++
	}

	if emails != 2 {
		t.Fatalf("3500 paths took %d emails, want 2", emails)
	}
}

// A file whose mtime predates its birth was copied with preserved times or backdated on purpose.
func TestBackdatedFilesAreFlagged(t *testing.T) {
	t.Parallel()

	backdated := readableDetail("/var/www/site/htdocs/wp-content/uploads/shell.php", "added", readableFTP,
		catalog.FieldChange{Field: readableMtime, Before: "0", After: "1697622241202511342"},
		catalog.FieldChange{Field: "btime", Before: "0 (known=false)", After: "1792230241202511342 (known=true)"})

	body := renderDefault(t, readableMessage(backdated))
	if !strings.Contains(body, "AVISO: mtime es 1095 d anterior a btime") {
		t.Fatal("backdated file not flagged\n" + body)
	}

	details := append(readableDetails(250), backdated)
	if compact := renderDefault(
		t,
		readableMessage(details...),
	); strings.Count(
		compact,
		"[mtime anterior a btime]",
	) != 1 {
		t.Fatal("compact listing must flag only the backdated path")
	}

	message := readableMessage()
	message.Report = &catalog.ReportInfo{
		Aggregate: nil, ID: "period", Summary: "", AIStatus: "disabled", From: 1, Until: 2, Part: 1,
		More: false, CoverageGap: false,
		Items: []catalog.ReportItem{{Detail: backdated, Head: "", Deferred: false, Violation: false}},
	}

	_, report, err := ReportText(message)
	if err != nil || !strings.Contains(report, "AVISO: mtime es 1095 d anterior a btime") {
		t.Fatal("report does not flag the backdated file", err, report)
	}
}
