package config_test

import (
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/config"
)

// repositoryIntegrity loads a configuration whose main file adds global and whose repositories web and
// system add web and system to their files, and returns the effective integrity of both.
func repositoryIntegrity(t *testing.T, global, web, system string) (config.Integrity, config.Integrity) {
	t.Helper()

	base := t.TempDir()
	for name, text := range map[string]string{
		mainFile:               "paths: {repositories: [services]}\n" + global,
		"services/web.yaml":    "type: db\npaths: [/srv/www]\n" + web,
		"services/system.yaml": "type: db\npaths: [/srv/etc]\n" + system,
	} {
		writeConfiguration(t, base, name, text)
	}

	cfg, err := config.Load(filepath.Join(base, mainFile))
	if err != nil {
		t.Fatal(err)
	}

	return cfg.ForRepository("web").Integrity, cfg.ForRepository("system").Integrity
}

// integrity.directories is inherited field by field: turning it off for one repository keeps that
// repository's hash settings and leaves every other repository tracking directories.
func TestIntegrityDirectoriesPerRepository(t *testing.T) {
	t.Parallel()

	web, system := repositoryIntegrity(t, "integrity: {hash: {algorithm: blake3}}\n",
		"integrity: {directories: false}\n", "")
	if web.Directories || !system.Directories {
		t.Fatalf("directories: web=%v system=%v, want false and true", web.Directories, system.Directories)
	}

	if web.Hash.Algorithm != config.HashBLAKE3 {
		t.Fatalf("turning directories off reset the hash settings: %+v", web)
	}

	web, system = repositoryIntegrity(t, "integrity: {directories: false}\n", "", "integrity: {directories: true}\n")
	if web.Directories || !system.Directories {
		t.Fatal("global integrity.directories not inherited or not overridable")
	}
}

// Attribute comparison is off by default and selected per repository without resetting the rest.
func TestIntegrityAttributesPerRepository(t *testing.T) {
	t.Parallel()

	web, system := repositoryIntegrity(t, "integrity: {hash: {algorithm: blake3}}\n",
		"integrity: {xattrs: true, acl: true}\n", "")
	if !web.Xattrs || !web.ACL || system.Xattrs || system.ACL {
		t.Fatalf("attributes: web=%+v system=%+v", web, system)
	}

	if web.Hash.Algorithm != config.HashBLAKE3 || !web.Directories {
		t.Fatalf("selecting attributes reset the other integrity settings: %+v", web)
	}
}
