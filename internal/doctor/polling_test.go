package doctor_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/doctor"
)

func TestPollingDoctorDoesNotProbeWatchers(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source")

	err := os.Mkdir(source, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	for name, contents := range map[string]string{
		"main.yaml":    "paths: {repositories: [profile.yaml]}\nwatch: {backend: polling}\n",
		"profile.yaml": "type: db\npaths: ['" + source + "']\n",
	} {
		err = os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := config.Load(filepath.Join(root, "main.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	report, err := doctor.Inspect(cfg)
	if err != nil || len(report.Paths) != 1 {
		t.Fatal(report, err)
	}

	if report.Paths[0].CapabilitiesProbed || report.FanotifyPIDFDLifetime != nil || len(report.Warnings) != 0 {
		t.Fatal("polling unexpectedly probed event attribution", report)
	}

	assertSourceUnchanged(t, source)
}
