package aisummary

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/config"
	gitrepo "github.com/inode64/fsledger/internal/git"
	"github.com/inode64/fsledger/internal/redact"
)

func stagedFixture(t *testing.T) (*gitrepo.Repository, map[string]string) {
	t.Helper()

	return stagedTexts(t, "TOKEN=secret-before\nVALUE=one\n", "TOKEN=secret-after\nVALUE=two\n")
}

func stagedTexts(t *testing.T, before, after string) (*gitrepo.Repository, map[string]string) {
	t.Helper()
	repository := &gitrepo.Repository{Path: t.TempDir(), Host: "ai-test", Timeout: 3 * time.Second}

	_, err := repository.Init(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(repository.Path, "config")

	err = os.WriteFile(path, []byte(before), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = repository.Commit(t.Context(), "initial")
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(path, []byte(after), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = repository.Stage(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	versions, err := repository.StagedVersions(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	return repository, versions
}

func TestMaskedPatchPreservesTextBoundaries(t *testing.T) {
	t.Parallel()

	for _, fixture := range []struct{ name, before, after, want string }{
		{"empty before", "", "added\n", "+added\n"},
		{"empty after", "removed\n", "", "-removed\n"},
		{"unicode", "valor=mañana\n", "valor=árbol🌳\n", "-valor=mañana\n+valor=árbol🌳\n"},
		{"no final newline", "old", "new", "-old\n\\ No newline at end of file\n+new\n"},
		{"newline only", "value", "value\n", "-value\n\\ No newline at end of file\n+value\n"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			repo, versions := stagedTexts(t, fixture.before, fixture.after)

			masker, err := redact.Compile(nil)
			if err != nil {
				t.Fatal(err)
			}

			patch, hidden, err := maskedPatch(t.Context(), repo, "/config", versions["/config"], masker)
			if err != nil || hidden || !strings.Contains(patch, fixture.want) {
				t.Fatalf("patch = %q, hidden = %v, error = %v", patch, hidden, err)
			}
		})
	}
}

func testProfile(endpoint string) config.AIProfile {
	return config.AIProfile{
		Provider: compatibleProvider, Endpoint: endpoint, Model: "summary-test",
		APIKeyEnv: "", Redact: nil, Timeout: 3 * time.Second, MaxDiffBytes: config.DefaultAIDiffBytes,
	}
}

func testSession(t *testing.T, manager *Manager, names ...string) *Session {
	t.Helper()

	var selection config.Repository

	selection.IACommit = names
	selection.IAInclude = []string{"/config"}

	session, err := manager.Session(&selection)
	if err != nil {
		t.Fatal(err)
	}

	return session
}

func TestFallbackRetainsRedactionAndCoolsDown(t *testing.T) {
	t.Parallel()
	repo, versions := stagedFixture(t)

	var failures, successes atomic.Int32

	first := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		failures.Add(1)
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer first.Close()

	bodies := make(chan string, 2)

	second := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		successes.Add(1)

		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}

		bodies <- string(body)

		writeReply(t, writer, openAIReply)
	}))
	defer second.Close()

	primary := testProfile(first.URL)
	primary.Redact = []redact.Rule{{Pattern: `(?m)^(TOKEN=).*$`, Replacement: `${1}[OCULTO]`}}
	manager := New(map[string]config.AIProfile{"first": primary, "second": testProfile(second.URL)})

	session := testSession(t, manager, "first", "second")
	for range 2 {
		text, status := session.Summarize(t.Context(), repo, versions)
		if text == "" || status != "summarized:second" {
			t.Fatal(text, status)
		}

		body := <-bodies
		if strings.Contains(body, "secret-before") || strings.Contains(body, "secret-after") ||
			!strings.Contains(body, "[OCULTO]") {
			t.Fatal("fallback lost primary redaction")
		}
	}

	if failures.Load() != 1 || successes.Load() != 2 {
		t.Fatal("cooldown not respected")
	}
}

func TestLocalPreparationFailureDoesNotCoolDownProviders(t *testing.T) {
	t.Parallel()
	repo, versions := stagedFixture(t)
	original := repo.Path

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeReply(t, writer, openAIReply)
	}))
	defer server.Close()

	manager := New(map[string]config.AIProfile{"first": testProfile(server.URL), "second": testProfile(server.URL)})
	session := testSession(t, manager, "first", "second")
	repo.Path = filepath.Join(t.TempDir(), "missing")

	text, status := session.Summarize(t.Context(), repo, versions)
	if text != "" || status != "unavailable" || len(session.cooldowns) != 0 {
		t.Fatal("local Git failure cooled down providers", text, status, session.cooldowns)
	}

	repo.Path = original

	text, status = session.Summarize(t.Context(), repo, versions)
	if text == "" || status != "summarized:first" {
		t.Fatal("provider remained unavailable after local recovery", text, status)
	}
}

func TestConcurrentBudgetSkipsWithoutBlockingCommit(t *testing.T) {
	t.Parallel()
	repo, versions := stagedFixture(t)
	entered := make(chan struct{}, 2)
	release := make(chan struct{}, 2)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		entered <- struct{}{}

		select {
		case <-release:
		case <-request.Context().Done():
		}

		writeReply(t, writer, openAIReply)
	}))
	defer server.Close()
	// Release handlers before closing the server even when an assertion fails.
	defer close(release)

	manager := New(map[string]config.AIProfile{"local": testProfile(server.URL)})
	completed := make(chan string, 2)

	for range 2 {
		session := testSession(t, manager, "local")
		go func() {
			_, status := session.Summarize(t.Context(), repo, versions)
			completed <- status
		}()
	}

	for range 2 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("requests failed to enter")
		}
	}

	third := testSession(t, manager, "local")

	text, status := third.Summarize(t.Context(), repo, versions)
	if text != "" || status != "busy" {
		t.Fatal("concurrency budget not enforced", text, status)
	}

	release <- struct{}{}

	release <- struct{}{}

	for range 2 {
		if status = <-completed; status != "summarized:local" {
			t.Fatal("active summary failed", status)
		}
	}

	if len(manager.slots) != 0 {
		t.Fatal("budget not released")
	}
}

func TestIncludesHaveNoImplicitSelections(t *testing.T) {
	t.Parallel()

	var session Session

	session.include = []string{"/etc/*.conf"}
	for _, path := range []string{"/etc/file.conf/secret", "/private/.git/config", "/etc/secret.key"} {
		if session.selected(path) {
			t.Fatal("unselected path permitted", path)
		}
	}

	if !session.selected("/etc/file.conf") {
		t.Fatal("selected path omitted")
	}
}
