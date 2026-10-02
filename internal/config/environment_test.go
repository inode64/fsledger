package config_test

import (
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/config"
)

func TestHostExpansionAcrossCatalogs(t *testing.T) {
	for _, host := range []string{"web01", "web02"} {
		t.Run(host, func(t *testing.T) {
			t.Setenv("HOST", host)
			base := t.TempDir()
			main := writeConfiguration(t, base, mainFile, `server: {name: "${HOST}"}
paths: {repositories: ["${HOST}.yaml"], excludes: [exclusions.yaml]}
storage: {git: {remote: "ssh://git@git.example/backup/config.git", branch: "${HOST}"}}
exclude: [exclusions]
`)
			writeConfiguration(t, base, host+".yaml", `paths: ["/srv/$HOST"]`)
			writeConfiguration(t, base, "exclusions.yaml", `["/srv/${HOST}/cache/**"]`)

			cfg, err := config.Load(main)
			if err != nil {
				t.Fatal(err)
			}

			repo := cfg.Repositories[host]
			if cfg.Server.Name != host || repo.Storage.Git.Branch != host || repo.Paths[0] != "/srv/"+host ||
				cfg.Exclude[0] != "/srv/"+host+"/cache/**" {
				t.Fatal("HOST not resolved consistently", cfg.Server.Name, repo)
			}
		})
	}
}

func TestHostExpansionCannotInjectYAML(t *testing.T) {
	t.Setenv("HOST", "value\nother: injected # ${HOME}")

	var value map[string]string

	err := config.ParseFileDocument(strings.NewReader("name: '${HOST}'\nunchanged: '${HOME}'"), &value)
	if err != nil || len(value) != 2 || value["name"] != "value\nother: injected # ${HOME}" ||
		value["unchanged"] != "${HOME}" {
		t.Fatal("environment changed document structure", value, err)
	}

	_, err = config.Parse(strings.NewReader("server: {name: '${HOST}'}\nunknown: true"))
	if err == nil {
		t.Fatal("HOST expansion disabled unknown field validation")
	}

	_, err = config.Parse(strings.NewReader("server: {name: '${HOST}'}\n---\n{}"))
	if err == nil {
		t.Fatal("HOST expansion discarded extra document")
	}
}

func TestPublishingConfigurationValidation(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		"branch: '../other'", "branch: 'a:other'", "branch: '-force'", "branch: 'HEAD'",
		"branch: 'a.lock'", "branch: 'a//b'", "branch: '.hidden'", "push_timeout: 0s", "push_interval: 0s",
		"remote: 'ext::helper'", "remote: 'https://user:password@git.example/repo'",
	} {
		_, err := config.Parse(strings.NewReader("storage: {git: {" + value + "}}"))
		if err == nil {
			t.Fatal("invalid publication accepted", value)
		}
	}
}
