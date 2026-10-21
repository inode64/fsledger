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
		{config.Template{Subject: checkedSubject, Body: "{{.Nope}}"}, config.NotificationChange, "", "Nope"},
		{config.Template{Subject: checkedSubject, Body: checkedFirst}, config.NotificationError, "", "index"},
		{
			config.Template{Subject: checkedSubject, Body: "{{.Report.ID}}"},
			config.NotificationChange, "", "nil pointer",
		},
		{
			config.Template{Subject: checkedSubject, Body: "{{(index .Details 1).Path}}"},
			config.NotificationIntegrityViolation, "", "index",
		},
		{
			// Digests merged from several operations share no change ID.
			config.Template{Subject: checkedSubject, Body: "{{if not .Detailed}}{{slice .ChangeID 0 8}}{{end}}"},
			config.NotificationChange, " of more than 250 paths", "slice",
		},
		{
			config.Template{Subject: checkedSubject + "\n", Body: "{{.Reason}}"},
			config.NotificationRecovery, "", "line break",
		},
	} {
		err := CheckTemplates(checkedConfig(test.definition, test.event))

		prefix := "notifier " + checkedNotifier + ": template " + checkedTemplate + " fails for " +
			test.event + " events" + test.sample
		if err == nil || !strings.Contains(err.Error(), prefix) || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%q for %s accepted or unexplained: %v", test.definition.Body, test.event, err)
		}
	}
}

// Only the events routed to a notifier are rendered, so a template may rely on paths that every
// change carries, and a template no notifier routes to is never rendered.
func TestCheckTemplatesFollowsRouting(t *testing.T) {
	t.Parallel()

	paths := config.Template{Subject: checkedSubject, Body: checkedFirst}

	err := CheckTemplates(checkedConfig(paths, config.NotificationChange, config.NotificationIntegrityViolation))
	if err != nil {
		t.Fatal("template valid for its routed events rejected:", err)
	}

	unrouted := checkedConfig(config.Template{Subject: checkedSubject, Body: "{{.Nope}}"})

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

	data, err := os.ReadFile("../../examples/templates/change.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var example config.Template

	err = yaml.Unmarshal(data, &example)
	if err != nil {
		t.Fatal(err)
	}

	events := []string{
		config.NotificationChange, config.NotificationIntegrityViolation,
		config.NotificationError, config.NotificationRecovery,
	}
	for _, definition := range []config.Template{
		example,
		{Subject: defaultSubject, Body: defaultBody},
		{Subject: "{{.Host}} {{.Hostname}}", Body: "added={{.Added}} modified={{.Modified}} deleted={{.Deleted}}"},
		// The fsledger.yaml(5) example.
		{
			Subject: "fsledger {{.Host}} / {{.Repository}}: {{.Event}} ({{.Count}})",
			Body: "Paths: {{.Count}} (+{{.Added}} ~{{.Modified}} -{{.Deleted}})\n" +
				"{{if .ChangeID}}Change: {{.ChangeID}}{{end}}\n",
		},
	} {
		err = CheckTemplates(checkedConfig(definition, events...))
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
