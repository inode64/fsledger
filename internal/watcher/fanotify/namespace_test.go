package fanotify_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/resource"
	"github.com/inode64/fsledger/internal/watcher/fanotify"
)

func TestKernelNamespace(t *testing.T) {
	t.Parallel()
	source := t.TempDir()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector, err := fanotify.New(source, matcher)
	if err != nil {
		t.Skipf("fanotify unavailable: %v", err)
	}
	defer resource.Close(detector)

	if detector.Mode() != "dfid_name" {
		t.Skipf("directory name handles unavailable: mode=%s", detector.Mode())
	}

	err = detector.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	old, next := filepath.Join(source, "before"), filepath.Join(source, "after")

	err = os.WriteFile(old, []byte("namespace fixture"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	awaitNamespace(t, detector, old, event.Create)

	err = os.Rename(old, next)
	if err != nil {
		t.Fatal(err)
	}

	raw := awaitNamespace(t, detector, next, event.Rename)
	if raw.OldPath != old {
		t.Fatalf("rename lost original path: %+v", raw)
	}

	//nolint:gosec // Verify executable-bit changes on a private test fixture.
	err = os.Chmod(next, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	awaitNamespace(t, detector, next, event.Attrib)

	err = os.Remove(next)
	if err != nil {
		t.Fatal(err)
	}

	awaitNamespace(t, detector, next, event.Remove)
}

// WordPress moves each plugin it updates into the excluded wp-content/upgrade-temp-backup. The excluded parent
// is not marked, so the kernel reports only the old name: the selection keeps its marks and loses nothing.
func TestKernelDirectoryMovedIntoExclusionKeepsCoverage(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	plugin, backup := filepath.Join(source, "plugin"), filepath.Join(source, "backup")

	for _, directory := range []string{filepath.Join(plugin, "includes"), backup} {
		err := os.MkdirAll(directory, 0o700)
		if err != nil {
			t.Fatal(err)
		}
	}

	matcher, err := exclude.New(backup + "/**")
	if err != nil {
		t.Fatal(err)
	}

	detector, err := fanotify.New(source, matcher)
	if err != nil {
		t.Skipf("fanotify unavailable: %v", err)
	}
	defer resource.Close(detector)

	if detector.Mode() != "dfid_name" {
		t.Skipf("directory name handles unavailable: mode=%s", detector.Mode())
	}

	err = detector.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	err = os.Rename(plugin, filepath.Join(backup, "plugin"))
	if err != nil {
		t.Fatal(err)
	}

	awaitNamespaceAny(t, detector, plugin, event.Rename, event.Remove)

	if detector.Dirty() || detector.PollingRequired() || !detector.ReconciliationPending() {
		t.Fatal("directory moved into an exclusion was reported as fanotify coverage loss")
	}

	// The moved directory keeps its marks; their events name excluded paths and must not surface.
	err = os.WriteFile(filepath.Join(backup, "plugin", "includes", "stray"), []byte("excluded"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	probe := filepath.Join(source, "probe")

	err = os.WriteFile(probe, []byte("still covered"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	raw := awaitNamespaceAny(t, detector, probe, event.Create)
	if detector.Dirty() {
		t.Fatal("coverage lost after the move", raw)
	}
}

// awaitNamespaceAny fails on any event for a path outside the selection while waiting.
func awaitNamespaceAny(t *testing.T, detector *fanotify.Watcher, path string, operations ...string) event.Raw {
	t.Helper()

	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()

	for {
		select {
		case raw := <-detector.Events():
			resource.OptionalFile(raw.PIDFD)

			raw.PIDFD = nil
			if strings.Contains(raw.Path, string(filepath.Separator)+"backup"+string(filepath.Separator)) {
				t.Fatalf("excluded path surfaced: %+v", raw)
			}

			if raw.Path == path && slices.Contains(operations, raw.Operation) {
				return *raw
			}
		case <-timer.C:
			t.Fatalf("missing %v for %s", operations, path)
		}
	}
}

func awaitNamespace(t *testing.T, detector *fanotify.Watcher, path, operation string) event.Raw {
	t.Helper()

	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()

	for {
		select {
		case raw := <-detector.Events():
			resource.OptionalFile(raw.PIDFD)

			raw.PIDFD = nil
			if raw.Path == path && raw.Operation == operation {
				if raw.PID != os.Getpid() {
					t.Fatalf("wrong actor PID: %+v", raw)
				}

				return *raw
			}
		case <-timer.C:
			t.Fatalf("missing %s for %s", operation, path)
		}
	}
}
