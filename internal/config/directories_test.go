package config_test

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/inode64/fsledger/internal/config"
)

// integrity.directories is inherited field by field: turning it off for one repository keeps that
// repository's comparison policy and leaves every other repository tracking directories.
func TestIntegrityDirectoriesPerRepository(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	for name, text := range map[string]string{
		mainFile: `paths: {repositories: [services]}
integrity: {reference: previous, compare: [hash, type, mode]}
`,
		"services/web.yaml": `type: db
paths: [/srv/www]
integrity: {directories: false}
`,
		"services/system.yaml": `type: db
paths: [/srv/etc]
`,
	} {
		writeConfiguration(t, base, name, text)
	}

	cfg, err := config.Load(filepath.Join(base, mainFile))
	if err != nil {
		t.Fatal(err)
	}

	web, system := cfg.ForRepository("web").Integrity, cfg.ForRepository("system").Integrity
	if web.Directories || !system.Directories {
		t.Fatalf("directories: web=%v system=%v, want false and true", web.Directories, system.Directories)
	}

	if web.Reference != config.ReferencePrevious || !slices.Equal(web.Compare, []string{"hash", "type", "mode"}) {
		t.Fatalf("turning directories off reset the comparison policy: %+v", web)
	}

	globalOff := t.TempDir()
	writeConfiguration(t, globalOff, mainFile, `paths: {repositories: [services]}
integrity: {directories: false}
`)
	writeConfiguration(t, globalOff, "services/web.yaml", "type: db\npaths: [/srv/www]\n")
	writeConfiguration(
		t,
		globalOff,
		"services/system.yaml",
		"type: db\npaths: [/srv/etc]\nintegrity: {directories: true}\n",
	)

	cfg, err = config.Load(filepath.Join(globalOff, mainFile))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.ForRepository("web").Integrity.Directories || !cfg.ForRepository("system").Integrity.Directories {
		t.Fatal("global integrity.directories not inherited or not overridable")
	}
}
