package proc_test

import (
	"os"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/attribution/proc"
	"github.com/inode64/fsledger/internal/event"
)

func TestSelfAndDisappearedProcess(t *testing.T) {
	t.Parallel()

	actor := proc.Resolve(t.Context(), event.Raw{PID: os.Getpid(), Time: time.Now(), SinceBoot: readLater(t)})
	//nolint:gosec // This test uses only its private temporary directory and the current test process.
	if !actor.Known || actor.StartTime == 0 || !actor.UserKnown || actor.UID != uint32(os.Getuid()) ||
		actor.Executable == "" {
		t.Fatalf("incomplete self: %+v", actor)
	}

	unknown := proc.Resolve(t.Context(), event.Raw{PID: 2147483647})
	if unknown.Known {
		t.Fatal("invented process identity")
	}
}

func TestHistoricalResolutionChecksBirthBeforeEnrichment(t *testing.T) {
	t.Parallel()

	raw := event.Raw{PID: os.Getpid(), SinceBoot: readLater(t)}
	live := proc.Resolve(t.Context(), raw)

	historical := proc.ResolveAt(t.Context(), raw, time.Now().Add(2*time.Second))
	if !live.Known || !historical.Known || live.StartTime != historical.StartTime {
		t.Fatalf("historical identity differs: live=%+v historical=%+v", live, historical)
	}

	if actor := proc.ResolveAt(t.Context(), raw, time.Unix(0, 0)); actor.Known {
		t.Fatal("current process attributed to an observation before its birth")
	}
}
