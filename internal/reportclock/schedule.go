// Package reportclock isolates calendar parsing from configuration and durable scheduling.
package reportclock

import (
	"strings"
	"time"

	"github.com/go-co-op/gocron/v2"

	"github.com/inode64/fsledger/internal/fault"
)

// Schedule uses five-field cron expressions in an explicit IANA timezone.
type Schedule struct{ calendar gocron.Cron }

// Parse excludes seconds, embedded timezone overrides and duration-based schedules.
func Parse(expression, zone string) (*Schedule, error) {
	if len(strings.Fields(expression)) != 5 || zone == "" || strings.ContainsAny(zone, " \t\n\r") {
		return nil, fault.New("a five-field cron schedule and an explicit timezone are required")
	}

	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fault.Wrap("schedule timezone", err)
	}

	// Validate once, then use only Next: durable scheduling remains owned by the caller.
	calendar := gocron.NewDefaultCron(false)

	err = calendar.IsValid(expression, location, time.Now())
	if err != nil {
		return nil, fault.Wrap("cron schedule", err)
	}

	return &Schedule{calendar: calendar}, nil
}

// Next returns the next absolute instant; nonexistent local times are skipped.
func (schedule *Schedule) Next(after time.Time) time.Time { return schedule.calendar.Next(after) }
