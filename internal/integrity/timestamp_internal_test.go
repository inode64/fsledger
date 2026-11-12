package integrity

import (
	"math"
	"testing"
	"time"
)

// Unix nanoseconds reach from the year 1677 to 2262. A time beyond either end is recorded as that end
// instead of wrapping round to an unrelated year.
func TestUnixNanoSaturatesOutsideItsRange(t *testing.T) {
	t.Parallel()

	const (
		second      = int64(time.Second)
		lastSecond  = math.MaxInt64 / second
		lastNano    = math.MaxInt64 % second
		firstSecond = math.MinInt64/second - 1
		firstNano   = second + math.MinInt64%second
	)

	year := func(year int) int64 { return time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC).Unix() }

	for _, fixture := range []struct {
		name            string
		sec, nsec, want int64
	}{
		{"epoch", 0, 0, 0},
		{"after the epoch", 1, 2, second + 2},
		{"before the epoch", -1, second - 1, -1},
		{"year 1700", year(1700), 1, year(1700)*second + 1},
		{"last instant", lastSecond, lastNano, math.MaxInt64},
		{"past the last instant", lastSecond, lastNano + 1, math.MaxInt64},
		{"year 2400", year(2400), 0, math.MaxInt64},
		{"latest second", math.MaxInt64, second - 1, math.MaxInt64},
		{"first instant", firstSecond, firstNano, math.MinInt64},
		{"after the first instant", firstSecond, firstNano + 1, math.MinInt64 + 1},
		{"before the first instant", firstSecond, firstNano - 1, math.MinInt64},
		{"year 1601", year(1601), 0, math.MinInt64},
		{"earliest second", math.MinInt64, 0, math.MinInt64},
	} {
		if got := unixNano(fixture.sec, fixture.nsec); got != fixture.want {
			t.Errorf("%s: %d s %d ns = %d, want %d", fixture.name, fixture.sec, fixture.nsec, got, fixture.want)
		}
	}
}
