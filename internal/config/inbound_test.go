package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
)

func TestBidirectionalConfiguration(t *testing.T) {
	t.Parallel()

	for _, invalid := range []string{"bidirectional: true", "fetch_interval: 0s", "fetch_timeout: -1s"} {
		_, err := config.Parse(strings.NewReader("storage: {git: {" + invalid + "}}"))
		if err == nil {
			t.Fatal("invalid synchronization configuration accepted", invalid)
		}
	}

	cfg, err := config.Parse(strings.NewReader("{}"))
	if err != nil || cfg.Storage.Git.Bidirectional || cfg.Storage.Git.FetchInterval != time.Minute ||
		cfg.Storage.Git.FetchTimeout != config.DefaultPushTimeout {
		t.Fatal("incorrect synchronization defaults", cfg, err)
	}
}

func TestBidirectionalInheritanceAndDBRejection(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	main := writeConfiguration(t, base, mainFile, `paths: {repositories: [one.yaml, two.yaml]}
storage: {git: {remote: origin, bidirectional: true, fetch_interval: 2m}}
`)
	writeConfiguration(t, base, "one.yaml", "paths: [/srv/one]")
	writeConfiguration(t, base, "two.yaml", `paths: [/srv/two]
type: db
storage: {git: {bidirectional: false, fetch_timeout: 45s}}`)

	cfg, err := config.Load(main)
	if err != nil {
		t.Fatal(err)
	}

	first, second := cfg.Repositories["one"].Storage.Git, cfg.Repositories["two"].Storage.Git
	if !first.Bidirectional || second.Bidirectional || second.FetchInterval != 2*time.Minute ||
		second.FetchTimeout != 45*time.Second {
		t.Fatal("incorrect field inheritance", first, second)
	}

	writeConfiguration(t, base, "two.yaml", "paths: [/srv/two]\ntype: db")

	_, err = config.Load(main)
	if err == nil {
		t.Fatal("DB repository accepted incoming Git writes")
	}
}
