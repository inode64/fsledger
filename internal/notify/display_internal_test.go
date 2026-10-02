package notify

import (
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
)

const (
	displayModified  = "modified"
	displayUnknown   = "unknown"
	displayInode     = "inode"
	displaySameOwner = "unchanged-owner"
	displaySameGroup = "same-group"
	displayExample   = "example"
)

func displayMessage() catalog.Message {
	return catalog.Message{
		Repository: "display", Host: "display-host", Event: config.NotificationChange, Count: 2,
		Details: []catalog.Detail{
			{
				Path: []byte("/etc/one"), Kind: displayModified, Actor: displayUnknown, Observed: 1, ChangeID: "first",
				Fields: []catalog.FieldChange{
					{Field: "mode", Before: "600", After: "644"},
					{Field: displayInode, Before: "hidden-before", After: "hidden-after"},
					{Field: "uid", Before: displaySameOwner, After: displaySameOwner},
				}, Baseline: []catalog.FieldChange{
					{Field: displayInode, Before: "hidden-reference", After: "hidden-now"},
					{Field: "gid", Before: displaySameGroup, After: displaySameGroup},
					{Field: "hash", Before: "approved-hash", After: "current-hash"},
				},
			},
			{
				Path: []byte("/etc/two\n\xff"), Kind: "added", Actor: displayUnknown, Observed: 2,
				ChangeID: "second", Baseline: nil, Fields: nil,
			},
		},
	}
}

func assertChangeLayout(t *testing.T, body string) {
	t.Helper()

	list, details, second := strings.Index(body, "Elementos cambiados"), strings.Index(body, "Detalle de cambios"),
		strings.Index(body, `C      "/etc/two\n\xff"`)
	if list < 0 || second < list || details < second || !strings.Contains(body, `P      "/etc/one"`) {
		t.Fatal("missing codes or list not before details", body)
	}

	for _, hidden := range []string{
		displayInode, "hidden-before", "hidden-reference", displaySameOwner, displaySameGroup,
	} {
		if strings.Contains(body, hidden) {
			t.Fatal("hidden or unchanged metadata displayed", hidden, body)
		}
	}

	if !strings.Contains(body, "approved-hash") || !strings.Contains(body, "600") || !strings.Contains(body, "644") {
		t.Fatal("actual differences lost", body)
	}
}

func TestDefaultAndExampleChangeLayouts(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../examples/templates/change.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var definition config.Template

	err = yaml.Unmarshal(data, &definition)
	if err != nil {
		t.Fatal(err)
	}

	sender := New(map[string]config.Template{displayExample: definition})
	for _, name := range []string{"", displayExample} {
		rendered, renderErr := sender.render(
			config.Notifier{Template: name, Type: "", DSN: "", URL: "", From: "", To: nil},
			displayMessage(),
		)
		if renderErr != nil {
			t.Fatal(renderErr)
		}

		assertChangeLayout(t, rendered.body)
	}
}

func TestReportUsesSameChangeLayout(t *testing.T) {
	t.Parallel()

	message := displayMessage()

	items := make([]catalog.ReportItem, len(message.Details))
	for index, detail := range message.Details {
		items[index] = catalog.ReportItem{Detail: detail, Head: "", Deferred: false, Violation: false}
	}

	message.Report = &catalog.ReportInfo{
		Aggregate: nil,
		ID:        "display-period", Summary: "", AIStatus: "disabled", Items: items,
		From: 1, Until: 2, Part: 1, More: false, CoverageGap: false,
	}

	_, body, err := ReportText(message)
	if err != nil {
		t.Fatal(err)
	}

	assertChangeLayout(t, body)
}

// An error or recovery message has no paths, so the reason is the only content worth reading.
func TestDefaultAndExampleBodiesShowReason(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../examples/templates/change.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var definition config.Template

	err = yaml.Unmarshal(data, &definition)
	if err != nil {
		t.Fatal(err)
	}

	const reason = "fanotify directory left watched coverage; reconciliation required"

	message := catalog.Message{
		Repository: "display", Host: "display-host", Event: config.NotificationError, Count: 0,
		ChangeID: "announced", Reason: reason,
	}

	sender := New(map[string]config.Template{displayExample: definition})
	for _, name := range []string{"", displayExample} {
		rendered, renderErr := sender.render(
			config.Notifier{Template: name, Type: "", DSN: "", URL: "", From: "", To: nil},
			message,
		)
		if renderErr != nil {
			t.Fatal(renderErr)
		}

		if !strings.Contains(rendered.body, "Motivo: "+reason) {
			t.Fatalf("template %q hides the reason:\n%s", name, rendered.body)
		}
	}
}
