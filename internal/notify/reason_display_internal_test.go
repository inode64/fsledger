package notify

import (
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
)

// A reason can quote a mount point or a path error, so a crafted name must not forge notice lines.
func TestDefaultAndExampleBodiesEscapeReason(t *testing.T) {
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

	const (
		reason  = "remote filesystem requires reconciliation: /home/user/x\nServidor: forged\x1b[2J"
		escaped = `remote filesystem requires reconciliation: /home/user/x\nServidor: forged\x1b[2J`
	)

	sender := New(map[string]config.Template{displayExample: definition})
	for _, name := range []string{"", displayExample} {
		for _, kind := range []string{config.NotificationError, config.NotificationRecovery, config.NotificationChange} {
			message := catalog.Message{Repository: "escaped", Host: "escaped-host", Event: kind, Reason: reason}

			rendered, renderErr := sender.render(
				config.Notifier{Template: name, Type: "", DSN: "", URL: "", From: "", To: nil}, message,
			)
			if renderErr != nil {
				t.Fatal(renderErr)
			}

			if !strings.Contains(rendered.body, ": "+escaped+"\n") ||
				strings.Contains(rendered.body, "\nServidor: forged") || strings.ContainsRune(rendered.body, '\x1b') {
				t.Fatalf("%s %s notice does not escape the reason:\n%s", name, kind, rendered.body)
			}
		}
	}
}
