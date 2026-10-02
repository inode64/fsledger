package config_test

import (
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/config"
)

func TestReportConfigurationValidation(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse(strings.NewReader("{}"))
	if err != nil || cfg.Reports.Enabled || cfg.Reports.AI.Enabled || cfg.Reports.Timezone != "UTC" {
		t.Fatal("invalid report defaults", cfg, err)
	}

	for _, invalid := range []string{
		"schedule: '@daily'", "schedule: '0 0 31 2 *'", "timezone: No/SuchZone",
		"enabled: true", "ai: {input: content}", "ai: {enabled: true}",
		"ai: {include: ['[']}", "ai: {include: [relative]}", "unknown_option: true",
	} {
		_, err = config.Parse(strings.NewReader("reports: {" + invalid + "}"))
		if err == nil {
			t.Fatal("accepted invalid reports", invalid)
		}
	}
}

func TestReportInheritanceAndEmailRouting(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	main := writeConfiguration(t, root, mainFile, `paths: {repositories: [repos], notifiers: [notifiers]}
reports:
  enabled: true
  use: [mail]
  timezone: Europe/Madrid
  ai: {enabled: true, profiles: [summary], include: ['/etc/**']}
`)
	writeConfiguration(t, root, "notifiers/mail.yaml", `type: email
dsn: smtps://mail.example.org
from: sender@example.org
to: [receiver@example.org]
`)
	writeConfiguration(t, root, "ai/summary.yaml", `provider: openai-compatible
endpoint: https://ai.example/v1
model: selected-model
`)
	writeConfiguration(t, root, "repos/one.yaml", "paths: [/etc]")
	writeConfiguration(t, root, "repos/two.yaml", `paths: [/srv]
type: db
reports: {enabled: false, use: [], ai: {enabled: false, include: []}}
`)

	cfg, err := config.Load(main)
	if err != nil {
		t.Fatal(err)
	}

	first, second := cfg.ForRepository("one").Reports, cfg.ForRepository("two").Reports
	if !first.Enabled || first.Timezone != "Europe/Madrid" || !first.AI.Enabled ||
		second.Enabled || len(second.Use) != 0 || len(second.AI.Include) != 0 || len(second.AI.Profiles) != 1 {
		t.Fatal("incorrect report inheritance", first, second)
	}

	writeConfiguration(t, root, "notifiers/mail.yaml", "type: slack\nurl: https://hooks.example.org/notify\n")

	_, err = config.Load(main)
	if err == nil {
		t.Fatal("non-email report destination accepted")
	}
}
