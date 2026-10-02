package doctor_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/doctor"
	"github.com/inode64/fsledger/internal/event"
)

func TestInspectReportsSelectedDetectorAndAbsentSources(t *testing.T) {
	t.Parallel()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	err = os.Mkdir(filepath.Join(root, "present"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	for name, text := range map[string]string{
		"main.yaml": "runtime: " + root + "/run\nstorage: {path: " + root + "/state}\n" +
			"paths: {repositories: [system.yaml]}\n",
		"system.yaml": "paths: ['" + root + "/present', '" + root + "/absent']\n",
	} {
		err = os.WriteFile(filepath.Join(root, name), []byte(text), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := config.Load(filepath.Join(root, "main.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	report, err := doctor.Inspect(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if len(report.Paths) != 1 || len(report.Missing["system"]) != 1 {
		t.Fatalf("unexpected sources: %+v", report)
	}

	path := report.Paths[0]
	// The mode is the one a watcher really installs, so it exists exactly when fanotify was selected.
	selected := path.Detector == event.Fanotify+"+"+event.Inotify
	if selected != (path.FanotifyMode != "") || (!selected && path.Detector != event.Inotify) || !path.Reconciliation {
		t.Fatalf("inconsistent diagnosis: %+v", path)
	}

	if path.FanotifyPIDFD != (report.FanotifyPIDFDLifetime != nil) {
		t.Fatalf("lifetime probe did not follow the selected backend: %+v", report)
	}

	assertSourceUnchanged(t, path.Path)
}

func assertSourceUnchanged(t *testing.T, path string) {
	t.Helper()

	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("doctor modified a source: %v %v", entries, err)
	}
}
