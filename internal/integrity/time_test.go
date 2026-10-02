package integrity_test

import (
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/integrity"
)

func TestImportedSecondPrecision(t *testing.T) {
	t.Parallel()

	fields := []string{"mtime"}

	for _, seconds := range []int64{-1, 0, 1700000000} {
		previous := integrity.Record{Mtime: seconds * int64(time.Second), TimesInSeconds: true}

		current := integrity.Record{Mtime: previous.Mtime + 1}
		if changed := integrity.Differences(previous, current, fields); len(changed) != 0 {
			t.Fatal("invented subsecond precision for AIDE", changed)
		}

		previous.TimesInSeconds = false
		if changed := integrity.Differences(previous, current, fields); len(changed) != 1 {
			t.Fatal("native nanosecond precision weakened", changed)
		}

		previous.TimesInSeconds = true

		current.Mtime = previous.Mtime + int64(time.Second)
		if changed := integrity.Differences(previous, current, fields); len(changed) != 1 {
			t.Fatal("full-second change missed", changed)
		}
	}
}
