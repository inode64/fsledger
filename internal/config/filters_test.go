package config_test

import (
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/config"
)

func TestFilterInheritance(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	main := writeConfiguration(t, root, mainFile, `paths: {repositories: [repos]}
notifications: {ignore: [{users: ['uid:1']}]}
commit: {defer: [{paths: ['/data/cache/**']}]}`)
	writeConfiguration(t, root, "repos/inherited.yaml", "paths: [/one]")
	writeConfiguration(t, root, "repos/replaced.yaml", `paths: [/two]
notifications: {ignore: [{users: ['uid:2'], command_regex: ['php','cron']}]}
commit: {defer: [{users: ['uid:3']}]}`)
	writeConfiguration(t, root, "repos/disabled.yaml", `paths: [/three]
notifications: {ignore: []}
commit: {defer: []}`)

	cfg, err := config.Load(main)
	if err != nil {
		t.Fatal(err)
	}

	inherited := cfg.ForRepository("inherited")
	replaced := cfg.ForRepository("replaced")
	disabled := cfg.ForRepository("disabled")

	if inherited.Notifications.Ignore[0].Users[0] != "uid:1" || inherited.Commit.Defer[0].Paths[0] != "/data/cache/**" {
		t.Fatal("lost inheritance")
	}

	if replaced.Notifications.Ignore[0].Users[0] != "uid:2" || replaced.Commit.Defer[0].Users[0] != "uid:3" {
		t.Fatal("lost override")
	}

	if len(disabled.Notifications.Ignore) != 0 || len(disabled.Commit.Defer) != 0 {
		t.Fatal("empty list failed to disable")
	}

	if cfg.Notifications.Ignore[0].Users[0] != "uid:1" {
		t.Fatal("repository mutated global rule")
	}
}

func TestStateSubtreesAreProtected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	main := writeConfiguration(t, root, mainFile, `runtime: /var/runtime
paths: {repositories: [repos]}
storage: {path: '/var/state[1]'}
`)
	writeConfiguration(t, root, "repos/one.yaml", "paths: [/var]")
	writeConfiguration(t, root, "repos/two.yaml", "paths: [/etc]\nstorage: {path: /var/other-state}")

	cfg, err := config.Load(main)
	if err != nil {
		t.Fatal(err)
	}

	matcher, err := cfg.Matcher("one")
	if err != nil {
		t.Fatal(err)
	}

	protected := []string{
		"/var/state[1]",
		"/var/state[1]/one/catalog.pebble/CURRENT",
		"/var/other-state/two/.git/index",
		"/var/runtime/one.json",
	}
	for _, path := range protected {
		if !matcher.Match(path) {
			t.Fatal("state is observable", path)
		}
	}

	for _, path := range []string{"/var/state1/source", "/var/state[1]-sibling/source"} {
		if matcher.Match(path) {
			t.Fatal("literal boundary lost", path)
		}
	}

	writeConfiguration(t, root, "repos/one.yaml", "paths: ['/var/state[1]/source']")

	_, err = config.Load(filepath.Join(root, mainFile))
	if err == nil {
		t.Fatal("source inside state accepted")
	}
}
