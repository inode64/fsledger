package daemon

import (
	"testing"
	"time"
)

func TestReconcileOffset(t *testing.T) {
	t.Parallel()

	for _, interval := range []time.Duration{time.Nanosecond, time.Second, 5 * time.Minute, 24 * time.Hour} {
		distinct := make(map[time.Duration]bool)

		for _, name := range []string{"backup", "admin", "web-a", "web-b", "localhost"} {
			offset := reconcileOffset(name, interval)
			if offset <= 0 || offset > interval || offset < interval/2 || offset != reconcileOffset(name, interval) {
				t.Fatal("invalid schedule", name, interval, offset)
			}

			distinct[offset] = true
		}

		if interval > time.Nanosecond && len(distinct) < 2 {
			t.Fatal("repositories not staggered")
		}
	}
}
