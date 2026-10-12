package cli

import (
	"testing"
	"time"
)

func TestParseRestoreDateAcrossClockChanges(t *testing.T) {
	t.Parallel()

	location, err := time.LoadLocation("Europe/Madrid")
	if err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		value string
		want  string
	}{
		{value: "2026-03-29", want: "2026-03-29T23:59:59+02:00"},
		{value: "2026-10-25", want: "2026-10-25T23:59:59+01:00"},
		{value: "2026-03-29 12:34", want: "2026-03-29T12:34:00+02:00"},
		{value: "2026-10-25 12:34:56", want: "2026-10-25T12:34:56+01:00"},
		{value: "2026-03-29T12:34:56Z", want: "2026-03-29T12:34:56Z"},
	} {
		t.Run(scenario.value, func(t *testing.T) {
			t.Parallel()

			when, parseErr := parseRestoreDate(scenario.value, location)
			if parseErr != nil {
				t.Fatal(parseErr)
			}

			if got := when.Format(time.RFC3339); got != scenario.want {
				t.Fatalf("got %s, want %s", got, scenario.want)
			}
		})
	}
}
