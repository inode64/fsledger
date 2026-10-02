package config

import (
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/redact"
	"github.com/inode64/fsledger/internal/reportclock"
)

// Reports independently selects scheduled email delivery for one repository.
type Reports struct {
	Format    string   `yaml:"format"`
	Schedule  string   `yaml:"schedule"`
	Timezone  string   `yaml:"timezone"`
	Use       []string `yaml:"use"`
	AI        ReportAI `yaml:"ai"`
	Enabled   bool     `yaml:"enabled"`
	SendEmpty bool     `yaml:"send_empty"`
}

// ReportAI grants explicit metadata-only access to selected paths.
type ReportAI struct {
	Input    string   `yaml:"input"`
	Profiles []string `yaml:"profiles"`
	Include  []string `yaml:"include"`
	Enabled  bool     `yaml:"enabled"`
}

func (settings Reports) validate() error {
	if settings.Format != "detailed" && settings.Format != "summary" {
		return fault.New("reports.format must be detailed or summary")
	}

	calendar, err := reportclock.Parse(settings.Schedule, settings.Timezone)
	if err != nil {
		return err
	}

	if calendar.Next(time.Now()).IsZero() {
		return fault.New("report schedule has no occurrence in the next five years")
	}

	if settings.Enabled && len(settings.Use) == 0 {
		return fault.New("enabled reports require email destinations in reports.use")
	}

	if settings.AI.Input != "metadata" {
		return fault.New("reports.ai.input currently supports only metadata")
	}

	if settings.AI.Enabled && (len(settings.AI.Profiles) == 0 || len(settings.AI.Include) == 0) {
		return fault.New("report AI requires profiles and an explicit include allowlist")
	}

	for _, pattern := range settings.AI.Include {
		if !strings.HasPrefix(pattern, "/") || !doublestar.ValidatePattern(pattern) {
			return fault.New("invalid report AI include pattern")
		}
	}

	return nil
}

func (c *Config) validateReports() error {
	for repository := range c.Repositories {
		settings := c.Repositories[repository].Reports
		for _, name := range settings.Use {
			notifier, exists := c.Notifiers[name]
			if !exists || notifier.Type != NotifierEmail {
				return fault.New("reports require an existing email notifier: " + name)
			}
		}

		var rules []redact.Rule

		for _, name := range settings.AI.Profiles {
			if _, exists := c.AI[name]; !exists {
				return fault.New("unknown report AI profile: " + name)
			}

			rules = append(rules, c.AI[name].Redact...)
		}

		_, err := redact.Compile(rules)
		if err != nil {
			return fault.Wrap("report AI redaction", err)
		}
	}

	return nil
}
