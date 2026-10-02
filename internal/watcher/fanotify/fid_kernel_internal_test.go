package fanotify

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/resource"
)

//nolint:funlen,gocognit // Keep mode setup, real kernel write and descriptor cleanup together.
func TestKernelFIDOnly(t *testing.T) {
	t.Parallel()
	source := t.TempDir()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector, err := New(source, matcher)
	if err != nil {
		t.Skipf("fanotify unavailable: %v", err)
	}
	defer resource.Close(detector)

	if detector.handles == nil {
		t.Skip("filesystem handles cannot be resolved")
	}

	err = unix.Close(detector.fd)
	if err != nil {
		t.Fatal(err)
	}

	detector.fd = -1

	err = detector.tryMode(unix.FAN_REPORT_FID, 0)
	if err != nil {
		t.Fatal(err)
	}

	err = detector.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(source, "fid-only")

	err = os.WriteFile(path, []byte("FID without a directory name record"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()

	for {
		select {
		case raw := <-detector.Events():
			resource.OptionalFile(raw.PIDFD)

			if raw.Path == path && raw.Operation == event.Write {
				if raw.PID != os.Getpid() {
					t.Fatal("FID-only event lost its actor")
				}

				return
			}
		case <-timer.C:
			t.Fatal("FID-only write event missing")
		}
	}
}
