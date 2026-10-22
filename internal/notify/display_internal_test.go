package notify

import (
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
)

const (
	displayModified = "modified"
	displayUnknown  = "unknown"
	displayInode    = "inode"
	displayExample  = "example"
)

func displayMessage() catalog.Message {
	return catalog.Message{
		Repository: "display", Host: "display-host", Event: config.NotificationChange, Count: 2,
		Details: []catalog.Detail{
			{
				Path: []byte("/etc/one"), Kind: displayModified, Actor: displayUnknown, Observed: 1, ChangeID: "first",
				Fields: []catalog.FieldChange{
					{Field: "mode", Before: "33152", After: "33188"},
					{Field: displayInode, Before: "hidden-before", After: "hidden-after"},
				},
				BackdatedBy: 0,
			},
			{
				Path: []byte("/etc/two\n\xff"), Kind: "added", Actor: displayUnknown, Observed: 2,
				ChangeID: "second", Fields: nil,
				BackdatedBy: 0,
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
		displayInode, "hidden-before",
	} {
		if strings.Contains(body, hidden) {
			t.Fatal("hidden or unchanged metadata displayed", hidden, body)
		}
	}

	if !strings.Contains(body, "-rw-------") || !strings.Contains(body, "-rw-r--r--") {
		t.Fatal("actual differences lost", body)
	}
}

func TestDefaultAndExampleChangeLayouts(t *testing.T) {
	t.Parallel()

	definition := exampleTemplate(t)

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
		items[index] = catalog.ReportItem{Detail: detail, Head: "", Deferred: false}
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

	definition := exampleTemplate(t)

	const reason = "fanotify directory left watched coverage; reconciliation required"

	sender := New(map[string]config.Template{displayExample: definition})
	for _, name := range []string{"", displayExample} {
		for _, kind := range []string{config.NotificationError, config.NotificationRecovery} {
			message := catalog.Message{
				Repository: "display", Host: "display-host", Event: kind,
				ChangeID: "announced", Reason: reason,
			}

			rendered, renderErr := sender.render(
				config.Notifier{Template: name, Type: "", DSN: "", URL: "", From: "", To: nil}, message,
			)
			if renderErr != nil {
				t.Fatal(renderErr)
			}

			assertOperationalLayout(t, rendered, message)
		}
	}
}

func assertOperationalLayout(t *testing.T, rendered renderedNotification, message catalog.Message) {
	t.Helper()

	state, label := "Estado: incidencia detectada.", "Motivo: "
	if message.Event == config.NotificationRecovery {
		state, label = "Estado: recuperado.", "Incidencia resuelta: "
	}

	for _, want := range []string{state, label + message.Reason, "Repositorio: display", "ID de aviso: announced"} {
		if !strings.Contains(rendered.body, want) {
			t.Fatalf("operational notice missing %q:\n%s", want, rendered.body)
		}
	}

	for _, unwanted := range []string{
		"Observaciones:", "Cambio:", "Elementos cambiados", "Detalle de cambios", "C = Creado",
	} {
		if strings.Contains(rendered.body, unwanted) {
			t.Fatalf("operational notice contains %q:\n%s", unwanted, rendered.body)
		}
	}

	if strings.Contains(rendered.subject, "(0:") || !strings.Contains(rendered.subject, message.Event) {
		t.Fatal("operational subject hides the event or shows change counters", rendered.subject)
	}
}
