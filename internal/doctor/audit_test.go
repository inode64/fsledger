package doctor_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/attribution/audit"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/doctor"
)

// The daemon subscribes to Audit from each repository's effective settings, never from the global default.
func TestInspectReportsAuditFromRepositorySettings(t *testing.T) {
	t.Parallel()

	probed := audit.Probe()
	disabled := audit.Status{Reason: "disabled in configuration", Available: probed.Available, Usable: false}

	for _, testCase := range []struct {
		name, global, repository string
		want                     audit.Status
	}{
		{
			name:   "enabled only by the repository",
			global: "attribution: {audit: {enabled: false}}\n", repository: "attribution: {audit: {enabled: true}}\n",
			want: probed,
		},
		{
			name:   "disabled by the repository",
			global: "", repository: "attribution: {audit: {enabled: false}}\n",
			want: disabled,
		},
		{name: "polling repository", global: "", repository: "watch: {backend: polling}\n", want: disabled},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			report, err := doctor.Inspect(loadAuditProfile(t, testCase.global, testCase.repository))
			if err != nil {
				t.Fatal(err)
			}

			if report.Audit != testCase.want {
				t.Fatalf("Audit reported as %+v, want %+v", report.Audit, testCase.want)
			}
		})
	}
}

// loadAuditProfile gives one repository an absent source, so Inspect probes nothing but Audit.
func loadAuditProfile(t *testing.T, global, repository string) *config.Config {
	t.Helper()

	const mainFile = "fsledger.yaml"

	root := t.TempDir()

	for name, contents := range map[string]string{
		mainFile:       "paths: {repositories: [profile.yaml]}\n" + global,
		"profile.yaml": "type: db\npaths: ['" + root + "/absent']\n" + repository,
	} {
		err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := config.Load(filepath.Join(root, mainFile))
	if err != nil {
		t.Fatal(err)
	}

	return cfg
}
