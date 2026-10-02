package config_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
)

func TestParse(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name, text string
		valid      bool
	}{
		{"defaults", "{}", true},
		{"old repositories", "repositories: {}", false},
		{"runtime", "runtime: /run/custom", true},
		{"old runtime object", "runtime: {path: /run/old}", false},
		{"runtime under paths", "paths: {runtime: /run/old}", false},
		{"old include", "include: []", false},
		{"unknown", "unexpected: yes", false},
		{"removed version field", "version: 2", false},
		{"bad duration", "commit: {debounce: invalid}", false},
		{"negative", "commit: {debounce: -1s}", false},
		{"delay", "commit: {debounce: 60s}", false},
		{"multiple documents", "{}\n---\n{}", false},
		{"false", "watch: {reconcile: {enabled: false}}", true},
		{"scan concurrency", "scan: {workers: 1, max_concurrent: 4}", true},
		{"zero scan concurrency", "scan: {max_concurrent: 0}", false},
		{"excess scan concurrency", "scan: {max_concurrent: 257}", false},
		{"zero workers", "scan: {workers: 0}", false},
		{"Git timeout", "storage: {git: {timeout: 20m}}", true},
		{"zero Git timeout", "storage: {git: {timeout: 0s}}", false},
		{"negative Git timeout", "storage: {git: {timeout: -1s}}", false},
		{"noncrypto baseline", "integrity: {hash: {algorithm: xxhash64}}", false},
		{"noncrypto previous", "integrity: {reference: previous, hash: {algorithm: xxhash64}}", true},
		{"notification digest defaults", "{}", true},
		{"notification maximum below window", "notifications: {batch_window: 2m, max_batch_window: 1m}", false},
		{"notification maximum equals window", "notifications: {batch_window: 2m, max_batch_window: 2m}", true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := config.Parse(strings.NewReader(testCase.text))
			if (err == nil) != testCase.valid {
				t.Fatalf("valid=%v error=%v", testCase.valid, err)
			}

			if err == nil && cfg.Commit.Debounce != 2*time.Second {
				t.Fatal("wrong debounce default")
			}

			if err == nil && testCase.name == "notification digest defaults" &&
				cfg.Notifications.MaxBatchWindow != 15*time.Minute {
				t.Fatal("wrong maximum notification window")
			}
		})
	}
}

func TestLoggingAndAuditOptions(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		options string
		valid   bool
	}{
		{"logging: {level: debug}", true},
		{"logging: {level: warning}", true},
		{"logging: {level: error, file: /var/log/fsledger/daemon.log}", true},
		{"logging: {level: trace}", false},
		{"logging: {level: ''}", false},
		{"logging: {file: daemon.log}", false},
		{"logging: {file: /var/lib/fsledger/repos/daemon.log}", false},
		{"logging: {file: /run/fsledger/daemon.log}", false},
		{"attribution: {audit: {key: 'invalid key'}}", false},
		{"attribution: {audit: {key: custom-key}}", true},
	} {
		t.Run(testCase.options, func(t *testing.T) {
			t.Parallel()

			_, err := config.Parse(strings.NewReader(testCase.options))
			if (err == nil) != testCase.valid {
				t.Fatalf("valid=%v error=%v", testCase.valid, err)
			}
		})
	}
}

func TestRepositoryRejections(t *testing.T) {
	t.Parallel()

	for _, text := range []string{
		"paths: []", "paths: [/etc, /etc]", "paths: [/etc, /etc/ssh]",
		"paths: [etc]", "paths: [/etc/../srv]", "paths: [/var/lib/fsledger/repos]",
		"paths: [/etc]\nname: ignored", "paths: [/etc]\ntype: unknown",
		"paths: [/etc]\nwatch: {reconcile: {interval: 0s}}",
		"paths: [/etc]\nnotifications: {use: [missing]}",
		"paths: ['/etc/**']", "paths: ['/etc/{ssh,ssl}']", "paths: ['/*/config']", "paths: ['/etc/[a']",
		"paths: ['/var/lib/fsledger/repos/*']", "paths: ['/etc/*/.git']",
	} {
		t.Run(text, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			path := writeConfiguration(t, base, mainFile, "paths: {repositories: [repo.yaml]}")
			writeConfiguration(t, base, "repo.yaml", text)

			_, err := config.Load(path)
			if err == nil {
				t.Fatal("invalid repository accepted")
			}
		})
	}
}

func TestMailDSN(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		text  string
		valid bool
	}{
		{"smtp://user:secret@example.org:587?tls=starttls", true},
		{"smtps://user:p%40ss@example.org:465", true},
		{"smtp://192.0.2.1:25?tls=starttls&tls_server_name=mail.example.org", true},
		{"smtps://192.0.2.1?tls_server_name=mail.example.org", true},
		{"smtps://example.org?tls_server_name=", false},
		{"smtps://example.org?tls_server_name=one&tls_server_name=two", false},
		{"smtps://example.org?tls_server_name=mail.example.org:25", false},
		{"smtps://example.org?tls=none", false},
		{"smtp://example.org", false},
		{"http://secret@example.org", false},
		{"smtp://example.org?tls=starttls&tls=none", false},
		{"smtp://secret%xx@example.org", false},
	} {
		endpoint, err := config.MailURL(testCase.text)
		if (err == nil) != testCase.valid {
			t.Fatalf("DSN validation: %v", err)
		}

		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Fatal("credential exposed")
		}

		if err == nil && endpoint == nil {
			t.Fatal("missing endpoint")
		}
	}
}

func FuzzConfig(f *testing.F) {
	f.Add("paths: {repositories: [repo.yaml]}")
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 65536 {
			t.Skip()
		}

		cfg, err := config.Parse(strings.NewReader(text))
		if err == nil && cfg == nil {
			t.Fatal("successful parse returned nil")
		}
	})
}

func TestSourceLogOverlap(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	path := writeConfiguration(
		t,
		base,
		mainFile,
		fmt.Sprintf("paths: {repositories: [repo.yaml]}\nlogging: {file: %q}", "/etc/log"),
	)
	writeConfiguration(t, base, "repo.yaml", "paths: [/etc]")

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("source/log overlap accepted")
	}
}
