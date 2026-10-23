package proc

import (
	"testing"
	"time"

	"github.com/prometheus/procfs"
)

func TestStartTimeAddsTicksToBootTime(t *testing.T) {
	t.Parallel()

	boot := time.Unix(1792000000, 0)

	if _, ok := startTime(0, boot); ok {
		t.Fatal("zero ticks must be unknown")
	}

	got, ok := startTime(17173045, boot)
	want := time.Unix(1792000000+171730, 450*int64(time.Millisecond))

	if !ok || !got.Equal(want) {
		t.Fatalf("startTime = %v, %v; want %v", got, ok, want)
	}
}

// The boot time derived from the clocks is /proc/stat btime, give or take the second a read may straddle.
func TestBootTimeMatchesProcStat(t *testing.T) {
	t.Parallel()

	fs, err := procfs.NewDefaultFS()
	if err != nil {
		t.Skip(err)
	}

	stat, err := fs.Stat()
	if err != nil {
		t.Skip(err)
	}

	boot, ok := bootTime()

	//nolint:gosec // btime is a Unix time in seconds.
	if !ok || boot.Sub(time.Unix(int64(stat.BootTime), 0)).Abs() > time.Second {
		t.Fatalf("boot time %v, /proc/stat btime %d", boot, stat.BootTime)
	}
}
