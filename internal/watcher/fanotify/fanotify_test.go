package fanotify_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	procattr "github.com/inode64/fsledger/internal/attribution/proc"
	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/resource"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/watcher/fanotify"
)

//nolint:gocognit,funlen // The full lifecycle keeps ordered filesystem/Git assertions together.
func TestKernelWrite(t *testing.T) {
	t.Parallel()
	source := t.TempDir()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector, err := fanotify.New(source, matcher)
	if err != nil {
		t.Skipf("fanotify not permitted/supported: %v", err)
	}
	defer resource.Close(detector)

	startErr := detector.Start(t.Context())
	if startErr != nil {
		if errors.Is(startErr, unix.EINVAL) {
			t.Fatalf("fanotify rejected backend flags after successful init: %v", startErr)
		}

		t.Skipf("fanotify marks unavailable: %v", startErr)
	}

	path := filepath.Join(source, "file")

	writeFileErr := os.WriteFile(path, []byte("changed"), 0o600)
	if writeFileErr != nil {
		t.Fatal(writeFileErr)
	}

	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()

	for {
		select {
		case raw := <-detector.Events():
			actor := (procattr.Provider{}).Resolve(t.Context(), *raw)

			hasPIDFD := raw.PIDFD != nil
			if raw.PIDFD != nil {
				err := raw.PIDFD.Close()
				if err != nil {
					t.Error(err)
				}
			}

			if raw.Path == path {
				if raw.PID != os.Getpid() {
					t.Fatalf("pid=%d want=%d", raw.PID, os.Getpid())
				}

				assertCurrentActor(t, actor)
				t.Logf(
					"kernel write attributed: pid=%d start=%d pidfd=%t mode=%s",
					raw.PID,
					actor.StartTime,
					hasPIDFD,
					detector.Mode(),
				)

				return
			}
		case <-timer.C:
			t.Fatal("fanotify write missing")
		}
	}
}

func assertCurrentActor(t *testing.T, actor event.Actor) {
	t.Helper()

	if !actor.Known || !actor.UserKnown || actor.PID != os.Getpid() || actor.StartTime == 0 ||
		int64(actor.UID) != int64(os.Getuid()) || int64(actor.EUID) != int64(os.Geteuid()) {
		t.Fatalf("incorrect current-process attribution: %+v", actor)
	}
}
