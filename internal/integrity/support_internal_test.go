package integrity

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestAtimeMaintainedFollowsMountOptions(t *testing.T) {
	t.Parallel()

	for _, fixture := range []struct {
		name      string
		flags     int64
		directory bool
		want      bool
	}{
		{"strictatime-file", 0, false, true},
		{"strictatime-directory", 0, true, true},
		{"noatime", unix.ST_NOATIME, false, false},
		{"relatime", unix.ST_RELATIME, false, false},
		{"nodiratime-file", unix.ST_NODIRATIME, false, true},
		{"nodiratime-directory", unix.ST_NODIRATIME, true, false},
		{"other-flags", unix.ST_NOSUID | unix.ST_NODEV, false, true},
	} {
		if got := atimeMaintained(fixture.flags, fixture.directory); got != fixture.want {
			t.Errorf("%s: atimeMaintained = %v, want %v", fixture.name, got, fixture.want)
		}
	}
}
