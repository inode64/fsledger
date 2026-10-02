package reportclock_test

import (
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/reportclock"
)

func TestCalendarAndDST(t *testing.T) {
	t.Parallel()

	schedule, err := reportclock.Parse("30 2 * * *", "Europe/Madrid")
	if err != nil {
		t.Fatal(err)
	}

	for _, fixture := range []struct{ after, next string }{
		{"2026-03-28T01:30:00Z", "2026-03-30T00:30:00Z"},
		{"2026-10-25T00:00:00Z", "2026-10-25T00:30:00Z"},
		{"2026-10-25T00:30:00Z", "2026-10-25T01:30:00Z"},
	} {
		after, parseErr := time.Parse(time.RFC3339, fixture.after)
		if parseErr != nil {
			t.Fatal(parseErr)
		}

		if next := schedule.Next(after).UTC().Format(time.RFC3339); next != fixture.next {
			t.Fatal(fixture, next)
		}
	}
}

func TestInvalidSchedules(t *testing.T) {
	t.Parallel()

	for _, expression := range []string{
		"@daily", "@every 1h", "0 0 8 * * *", "61 * * * *", "0 0 31 2 *",
		"CRON_TZ=UTC 0 8 * * *", "TZ=UTC 0 8 * * *",
	} {
		_, err := reportclock.Parse(expression, "UTC")
		if err == nil {
			t.Fatal("accepted", expression)
		}
	}

	for _, zone := range []string{"", "No/SuchZone", "UTC CRON_TZ=GMT"} {
		_, err := reportclock.Parse("0 8 * * *", zone)
		if err == nil {
			t.Fatal("accepted", zone)
		}
	}
}
