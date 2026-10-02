package daemon

import (
	"hash/crc32"
	"math"
	"time"
)

// reconcileOffset spreads the first periodic scan without delaying startup or
// changing the full-hash timer. Subsequent scans keep the configured interval.
func reconcileOffset(name string, interval time.Duration) time.Duration {
	const halves = 2

	fraction := float64(crc32.ChecksumIEEE([]byte(name))) / float64(math.MaxUint32)

	return interval - time.Duration(float64(interval/halves)*fraction)
}
