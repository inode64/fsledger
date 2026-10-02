package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
)

func TestScanScheduleValidation(t *testing.T) {
	t.Parallel()

	for _, document := range []string{
		"watch: {reconcile: {interval: 1h, schedule: '* * * * *'}}",
		"watch: {reconcile: {schedule: '@daily'}}",
		"watch: {reconcile: {schedule: '0 0 31 2 *'}}",
		"watch: {reconcile: {schedule: '* * * * *', timezone: Invalid/Zone}}",
		"watch: {reconcile: {on_start: false}}",
		"watch: {reconcile: {on_stop: false}}",
		"watch: {reconcile: {schedule: null}}",
		"watch: {reconcile: {interval: null}}",
		"watch: {reconcile: {schedule: '* * * * *', unknown: true}}",
		"integrity: {hash: {full_scan_interval: 1h, full_scan_schedule: '* * * * *'}}",
		"integrity: {hash: {full_scan_schedule: ''}}",
		"integrity: {hash: {full_scan_schedule: '* * * * *', full_scan_timezone: ''}}",
		"reports: {format: invalid}",
	} {
		_, err := config.Parse(strings.NewReader(document))
		if err == nil {
			t.Fatal("accepted invalid schedule", document)
		}
	}
}

func TestScanScheduleInheritance(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	main := writeConfiguration(t, root, mainFile, `paths: {repositories: [repos]}
watch:
  backend: polling
  reconcile: {schedule: '15 3 * * *', timezone: Europe/Madrid, on_start: false, on_stop: false}
integrity:
  hash: {full_scan_schedule: '15 3 * * *', full_scan_timezone: Europe/Madrid}
reports: {format: summary}
`)
	writeConfiguration(t, root, "repos/one.yaml", "type: db\npaths: [/srv/one]")
	writeConfiguration(t, root, "repos/two.yaml", `type: db
paths: [/srv/two]
watch: {reconcile: {interval: 2h}}
integrity: {hash: {full_scan_interval: 24h}}
`)
	writeConfiguration(t, root, "repos/three.yaml", `paths: [/srv/three]
watch: {backend: auto, reconcile: {interval: 1h, on_start: true, on_stop: true}}
`)

	cfg, err := config.Load(main)
	if err != nil {
		t.Fatal(err)
	}

	first, second := cfg.ForRepository("one"), cfg.ForRepository("two")
	if first.Watch.Reconcile.Interval != 0 || first.Integrity.Hash.FullScanInterval != 0 ||
		first.Watch.Reconcile.OnStart || first.Watch.Reconcile.OnStop || first.Reports.Format != "summary" {
		t.Fatal("cron did not replace inherited intervals", first)
	}

	if second.Watch.Reconcile.Schedule != "" || second.Watch.Reconcile.Interval != 2*time.Hour ||
		second.Integrity.Hash.FullScanSchedule != "" || second.Watch.Reconcile.Timezone != "Europe/Madrid" {
		t.Fatal("interval did not replace inherited cron", second)
	}
}
