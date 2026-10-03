package integrity

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func TestBirthTimeLookupFailureIsNotMissingMetadata(t *testing.T) {
	t.Parallel()

	record := Record{HasBtime: true, Btime: 123}

	err := readBirthTime(-1, "file", &record)
	if !errors.Is(err, unix.EBADF) {
		t.Fatal("failed birth-time lookup accepted as unsupported", err)
	}

	if !record.HasBtime || record.Btime != 123 {
		t.Fatal("failed lookup overwrote birth time")
	}
}
