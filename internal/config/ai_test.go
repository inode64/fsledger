package config_test

import (
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/config"
)

func TestAIProfilesAndRepositorySelection(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	main := writeConfiguration(t, root, mainFile, "paths: {repositories: [repos]}")
	writeConfiguration(t, root, "ai/gpt.yaml", `provider: openai-compatible
endpoint: https://ai.example/v1
model: selected-model
redact:
  - pattern: '(TOKEN=).*'
    replacement: '${1}[OCULTO]'
`)
	writeConfiguration(t, root, "repos/one.yaml", "paths: [/one]\nia_commit: [gpt]\nia_include: ['/one/**']")
	writeConfiguration(t, root, "repos/two.yaml", "paths: [/two]")

	cfg, err := config.Load(main)
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Repositories["one"].IACommit) != 1 || len(cfg.Repositories["two"].IACommit) != 0 ||
		cfg.AI["gpt"].MaxDiffBytes != config.DefaultAIDiffBytes {
		t.Fatal("incorrect repository routing")
	}

	for _, body := range []string{
		"paths: [/one]\nia_commit: [missing]",
		"paths: [/one]\nia_commit: [gpt,gpt]",
		"type: db\npaths: [/one]\nia_commit: [gpt]",
		"paths: [/one]\nia_include: [relative]",
	} {
		writeConfiguration(t, root, "repos/one.yaml", body)

		_, err = config.Load(main)
		if err == nil {
			t.Fatal("invalid AI selection accepted", body)
		}
	}

	writeConfiguration(t, root, "repos/one.yaml", "paths: [/one]")
	writeConfiguration(
		t,
		root,
		"ai/gpt.yaml",
		"provider: openai\nendpoint: https://example.test\nmodel: model\npaths: [/one]",
	)

	_, err = config.Load(filepath.Join(root, mainFile))
	if err == nil {
		t.Fatal("paths accepted in AI profile")
	}
}
