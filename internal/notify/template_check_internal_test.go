package notify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/inode64/fsledger/internal/config"
)

const (
	checkedNotifier = "ops"
	checkedTemplate = "custom"
	checkedSubject  = "fsledger {{.Host}}: {{.Event}}"
	checkedFirst    = "{{index .Paths 0}}"
)

func checkedConfig(definition config.Template, events ...string) *config.Config {
	var repository config.Repository

	repository.Notifications.Use = []string{checkedNotifier}
	repository.Notifications.Events = events

	cfg := &config.Config{}
	cfg.Repositories = map[string]config.Repository{"system": repository}
	cfg.Templates = map[string]config.Template{checkedTemplate: definition}
	cfg.Notifiers = map[string]config.Notifier{checkedNotifier: {
		Type: config.NotifierSlack, URL: "https://hooks.example.org/services/x", DSN: "", From: "",
		Template: checkedTemplate, To: nil,
	}}

	return cfg
}

// Parsing accepts these templates; only executing them shows that every delivery would fail.
func TestCheckTemplatesRejectsRenderFailures(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		definition config.Template
		event      string
		sample     string
		want       string
	}{
		{parsedTemplate(t, checkedSubject, "{{.Nope}}"), config.NotificationChange, "", "Nope"},
		{parsedTemplate(t, checkedSubject, checkedFirst), config.NotificationError, "", "index"},
		{
			parsedTemplate(t, checkedSubject, "{{.Report.ID}}"),
			config.NotificationChange, "", "nil pointer",
		},
		{
			// Digests merged from several operations share no change ID.
			parsedTemplate(t, checkedSubject, "{{if not .Detailed}}{{slice .ChangeID 0 8}}{{end}}"),
			config.NotificationChange, " of more than 250 paths", "slice",
		},
		{
			parsedTemplate(t, checkedSubject+"\n", "{{.Reason}}"),
			config.NotificationRecovery, "", "line break",
		},
	} {
		err := CheckTemplates(checkedConfig(test.definition, test.event))

		prefix := "notifier " + checkedNotifier + ": template " + checkedTemplate + " fails for " +
			test.event + " events" + test.sample
		if err == nil || !strings.Contains(err.Error(), prefix) || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%q for %s accepted or unexplained: %v", test.definition.Body.Root.String(), test.event, err)
		}
	}
}

// Only the events routed to a notifier are rendered, so a template may rely on paths that every
// change carries, and a template no notifier routes to is never rendered.
func TestCheckTemplatesFollowsRouting(t *testing.T) {
	t.Parallel()

	paths := parsedTemplate(t, checkedSubject, checkedFirst)

	err := CheckTemplates(checkedConfig(paths, config.NotificationChange))
	if err != nil {
		t.Fatal("template valid for its routed events rejected:", err)
	}

	unrouted := checkedConfig(parsedTemplate(t, checkedSubject, "{{.Nope}}"))

	err = CheckTemplates(unrouted)
	if err != nil {
		t.Fatal("template of a notifier without routed events rendered:", err)
	}

	err = CheckTemplates(checkedConfig(paths, config.NotificationChange, config.NotificationError))
	if err == nil || !strings.Contains(err.Error(), "fails for "+config.NotificationError+" events") {
		t.Fatal("operational event without paths not rendered:", err)
	}
}

// Samples must stay realistic: the built-in template, the shipped example and the summary
// variables documented for templates render for every event.
func TestCheckTemplatesAcceptsShippedTemplates(t *testing.T) {
	t.Parallel()

	example := exampleTemplate(t)

	events := []string{config.NotificationChange, config.NotificationError, config.NotificationRecovery}
	for _, definition := range []config.Template{
		example,
		parsedTemplate(t, defaultSubject, defaultBody),
		parsedTemplate(t, "{{.Host}} {{.Hostname}}", "added={{.Added}} modified={{.Modified}} deleted={{.Deleted}}"),
		// The fsledger.yaml(5) example.
		parsedTemplate(
			t,
			"fsledger {{.Host}} / {{.Repository}}: {{.Event}} ({{.Count}})",
			"Paths: {{.Count}} (+{{.Added}} ~{{.Modified}} -{{.Deleted}})\n"+
				"{{if .ChangeID}}Change: {{.ChangeID}}{{end}}\n",
		),
	} {
		err := CheckTemplates(checkedConfig(definition, events...))
		if err != nil {
			t.Error(err)
		}
	}
}

// The installed example routes operational events to a notifier with the example template.
func TestCheckTemplatesAcceptsExampleConfiguration(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(filepath.Join("..", "..", "examples", "fsledger.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	err = CheckTemplates(cfg)
	if err != nil {
		t.Fatal(err)
	}
}

func parsedTemplate(t *testing.T, subject, body string) config.Template {
	t.Helper()

	parsed, err := config.ParseTemplate("test", subject, body)
	if err != nil {
		t.Fatal(err)
	}

	return parsed
}

func exampleTemplate(t *testing.T) config.Template {
	t.Helper()

	data, err := os.ReadFile("../../examples/templates/change.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var definition struct {
		Subject string `yaml:"subject"`
		Body    string `yaml:"body"`
	}

	err = yaml.Unmarshal(data, &definition)
	if err != nil {
		t.Fatal(err)
	}

	return parsedTemplate(t, definition.Subject, definition.Body)
}
