package config

import (
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/reportclock"
)

// Reset only the inherited alternative selected in this document. The ordinary
// KnownFields decoder still validates the entire document afterwards, including
// unknown fields. YAML's own decoder handles aliases, merges and duplicate keys.
func resetScanSchedules(data []byte, target any) error {
	var (
		watching *Reconcile
		hashing  *Hash
	)

	switch settings := target.(type) {
	case *Config:
		watching, hashing = &settings.Watch.Reconcile, &settings.Integrity.Hash
	case *Repository:
		watching, hashing = &settings.Watch.Reconcile, &settings.Integrity.Hash
	default:
		return nil
	}

	var fields struct {
		Watch struct {
			Reconcile map[string]any `yaml:"reconcile"`
		} `yaml:"watch"`
		Integrity struct {
			Hash map[string]any `yaml:"hash"`
		} `yaml:"integrity"`
	}

	err := yaml.Unmarshal(data, &fields)
	if err != nil {
		return fault.Wrap("read scan schedule selection", err)
	}

	err = resetScanChoice(fields.Watch.Reconcile, "interval", "schedule", &watching.Interval, &watching.Schedule)
	if err != nil {
		return err
	}

	return resetScanChoice(fields.Integrity.Hash, "full_scan_interval", "full_scan_schedule",
		&hashing.FullScanInterval, &hashing.FullScanSchedule)
}

func resetScanChoice(
	fields map[string]any,
	intervalKey, scheduleKey string,
	interval *time.Duration,
	schedule *string,
) error {
	intervalValue, hasInterval := fields[intervalKey]

	scheduleValue, hasSchedule := fields[scheduleKey]
	if hasInterval && hasSchedule {
		return fault.New(intervalKey + " and " + scheduleKey + " are mutually exclusive")
	}

	if hasInterval {
		if intervalValue == nil {
			return fault.New(intervalKey + " must not be null")
		}

		*schedule = ""
	}

	if hasSchedule {
		if scheduleValue == nil {
			return fault.New(scheduleKey + " must not be null")
		}

		*interval = 0
	}

	return nil
}

func validateScanSchedule(interval time.Duration, expression, zone string) error {
	if expression == "" {
		if interval <= 0 {
			return fault.New("scan interval must be positive or replaced by a cron schedule")
		}

		return nil
	}

	if interval != 0 {
		return fault.New("scan interval and cron schedule are mutually exclusive")
	}

	calendar, err := reportclock.Parse(expression, zone)
	if err != nil {
		return err
	}

	if calendar.Next(time.Now()).IsZero() {
		return fault.New("scan schedule has no occurrence in the next five years")
	}

	return nil
}
