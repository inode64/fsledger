package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/config"
)

const mainFile = "main.yaml"

func writeConfiguration(t *testing.T, base, name, text string) string {
	t.Helper()

	path := filepath.Join(base, name)

	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(path, []byte(text), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

//nolint:cyclop,funlen,gocognit,gocyclo // Ordered integration fixture keeps setup, transitions and assertions together.
func TestCatalogInheritance(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	//nolint:gosec // Dummy DSN credentials exercise parsing in an isolated fixture.
	for name, text := range map[string]string{
		mainFile: `paths:
  repositories: [web.yaml, services]
  notifiers: [notifiers]
  excludes: [excludes, excludes.apache]
  templates: templates
exclude: [common]
notifications: {use: [email], events: [change], max_batch_window: 12m}
watch: {reconcile: {enabled: true, interval: 30s}}
storage: {git: {timeout: 20m}}
`,
		"web.yaml": `paths: [/srv/web]
exclude: [excludes.apache]
watch: {reconcile: {enabled: false}}
notifications: {use: []}
storage: {path: /srv/history}`,
		"services/db.yml": `type: db
paths: [/srv/database]
storage: {git: {timeout: 40m}}
watch: {backend: polling}
attribution: {audit: {enabled: false}}`,
		"services/ignored.txt": "bad yaml: [", "services/nested/ignored.yaml": "bad yaml: [",
		"notifiers/email.yaml": `type: email
dsn: 'smtps://user:pass@example.org:465'
from: a@example.org
to: [b@example.org]
template: email`,
		"templates/email.yaml": `subject: 'Changes in {{.Repository}}'
body: 'Changed paths: {{.Count}}'`,
		"excludes/common.yaml": "- '**/*.secret'",
		"excludes.apache":      "- '**/cache/**'",
	} {
		writeConfiguration(t, base, name, text)
	}

	cfg, err := config.Load(filepath.Join(base, mainFile))
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(cfg.Names(), []string{"db", "web"}) {
		t.Fatal(cfg.Names())
	}

	if cfg.Templates["email"].Subject != "Changes in {{.Repository}}" ||
		cfg.Templates["email"].Body != "Changed paths: {{.Count}}" {
		t.Fatal("template document not loaded")
	}

	web := cfg.ForRepository("web")
	if web.Watch.Reconcile.Enabled || web.Watch.Reconcile.Interval != 30*time.Second ||
		len(web.Notifications.Use) != 0 {
		t.Fatal("false/list inheritance lost")
	}

	if web.Storage.Path != "/srv/history" ||
		web.Storage.Git.Timeout != 20*time.Minute {
		t.Fatal("storage inheritance lost")
	}

	database := cfg.ForRepository("db")
	if database.Storage.Git.Timeout != 40*time.Minute || cfg.Storage.Git.Timeout != 20*time.Minute {
		t.Fatal("Git timeout override leaked")
	}

	if database.Watch.Backend != "polling" || !database.Watch.Reconcile.Enabled || database.Attribution.Audit.Enabled ||
		len(database.Notifications.Use) != 1 || database.Notifications.MaxBatchWindow != 12*time.Minute {
		t.Fatal("repository overrides leaked")
	}

	matcher, err := cfg.Matcher("web")
	if err != nil {
		t.Fatal(err)
	}

	if !matcher.Match("/srv/web/a.secret") || !matcher.Match("/srv/web/cache/a") || matcher.Match("/srv/web/index") {
		t.Fatal("profiles not combined")
	}
}

func TestTemplateDocumentsAreStrict(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name, document string
	}{
		{"plain text", "legacy {{.Repository}}"},
		{"missing subject", "body: '{{.Repository}}'"},
		{"missing body", "subject: '{{.Repository}}'"},
		{"unknown field", "subject: ok\nbody: ok\nhtml: no"},
		{"invalid subject", "subject: '{{'\nbody: ok"},
		{"invalid body", "subject: ok\nbody: '{{'"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			path := writeConfiguration(t, base, mainFile, `paths:
  repositories: [repo.yaml]
  templates: templates`)
			writeConfiguration(t, base, "repo.yaml", "paths: [/srv/source]")
			writeConfiguration(t, base, "templates/change.yaml", testCase.document)

			_, err := config.Load(path)
			if err == nil {
				t.Fatal("invalid template document accepted")
			}
		})
	}
}

func TestCatalogRejections(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct{ name, main, fragment, want string }{
		{"missing", "paths: {repositories: [missing.yaml]}", "", "missing.yaml"},
		{"empty", "paths: {repositories: ['']}", "", "must not be empty"},
		{"bad YAML", singleCatalog, "paths: [", "repo.yaml"},
		{"unknown", singleCatalog, "paths: [/etc]\ntyop: true", "tyop"},
		{"include", singleCatalog, "include: [other.yaml]", "include"},
		{"envelope", singleCatalog, "repositories: {}", "repositories"},
		{"duplicate", "paths: {repositories: [repo.yaml, repo.yaml]}", "paths: [/etc]", "duplicate repository"},
		{"documents", singleCatalog, "paths: [/etc]\n---\npaths: [/srv]", "one YAML document"},
		{"no repos", "{}", "", "at least one repository"},
		{"profile", singleCatalog, "paths: [/etc]\nexclude: [missing]", "unknown exclusion"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()

			path := writeConfiguration(t, base, mainFile, testCase.main)
			if testCase.fragment != "" {
				writeConfiguration(t, base, "repo.yaml", testCase.fragment)
			}

			_, err := config.Load(path)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("want %s, got %v", testCase.want, err)
			}
		})
	}
}

//nolint:cyclop,funlen,gocognit,gocyclo // Ordered integration fixture keeps setup, transitions and assertions together.
func TestCatalogAliasesAndLimits(t *testing.T) {
	t.Parallel()
	t.Run("same name", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		path := writeConfiguration(t, base, mainFile, "paths: {repositories: [a, b]}")
		writeConfiguration(t, base, "a/repo.yaml", "paths: [/srv/a]")
		writeConfiguration(t, base, "b/repo.yml", "paths: [/srv/b]")

		_, err := config.Load(path)
		if err == nil || !strings.Contains(err.Error(), "duplicate repository") {
			t.Fatal(err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()

		source := writeConfiguration(t, base, "repo.yaml", "paths: [/srv/a]")

		err := os.Symlink(source, filepath.Join(base, "alias.yaml"))
		if err != nil {
			t.Fatal(err)
		}

		path := writeConfiguration(t, base, mainFile, "paths: {repositories: [alias.yaml]}")

		cfg, err := config.Load(path)
		if err != nil || !slices.Equal(cfg.Names(), []string{"alias"}) {
			t.Fatalf("%v %v", cfg, err)
		}

		writeConfiguration(t, base, mainFile, "paths: {repositories: [alias.yaml, repo.yaml]}")

		_, err = config.Load(path)
		if err == nil || !strings.Contains(err.Error(), "repeated configuration") {
			t.Fatal(err)
		}
	})
	t.Run("fifo", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()

		path := writeConfiguration(t, base, mainFile, "paths: {repositories: [pipe.yaml]}")

		err := unix.Mkfifo(filepath.Join(base, "pipe.yaml"), 0o600)
		if err != nil {
			t.Fatal(err)
		}

		_, err = config.Load(path)
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatal(err)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()

		path := writeConfiguration(t, base, mainFile, "paths: {repositories: [a.yaml, b.yaml]}")
		for _, name := range []string{"a", "b"} {
			writeConfiguration(t, base, name+".yaml", "#"+strings.Repeat("x", 2<<20)+"\npaths: [/srv/"+name+"]")
		}

		_, err := config.Load(path)
		if err == nil || !strings.Contains(err.Error(), "4 MiB") {
			t.Fatal(err)
		}
	})
	t.Run("files", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()

		path := writeConfiguration(t, base, mainFile, "paths: {repositories: [many]}")
		for index := range 256 {
			writeConfiguration(t, base, fmt.Sprintf("many/%03d.yaml", index), fmt.Sprintf("paths: [/srv/%d]", index))
		}

		_, err := config.Load(path)
		if err == nil || !strings.Contains(err.Error(), "256 files") {
			t.Fatal(err)
		}
	})
}

const singleCatalog = "paths: {repositories: [repo.yaml]}"
