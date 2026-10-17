package proc

import (
	"testing"
	"time"
)

func TestStartTimeAddsTicksToBootTime(t *testing.T) {
	t.Parallel()

	if _, ok := startTime(0, 1792000000); ok {
		t.Fatal("zero ticks must be unknown")
	}

	if _, ok := startTime(100, 0); ok {
		t.Fatal("unknown boot time must be unknown")
	}

	got, ok := startTime(17173045, 1792000000)
	want := time.Unix(1792000000+171730, 450*int64(time.Millisecond))

	if !ok || !got.Equal(want) {
		t.Fatalf("startTime = %v, %v; want %v", got, ok, want)
	}
}
