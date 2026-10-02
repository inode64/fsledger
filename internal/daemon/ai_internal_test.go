package daemon

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/aisummary"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/filter"
	"github.com/inode64/fsledger/internal/redact"
)

const summaryProfile = "gpt"

//nolint:funlen,cyclop,gocognit,gocyclo // Ordered integration of deferred, masked and committed versions.
func TestAISummaryUsesMaskedCombinedIndex(t *testing.T) {
	t.Parallel()
	runner, noise, normal := deferFixture(
		t,
		[]filter.Rule{{Paths: []string{noisePattern}, Users: nil, CommandRegex: nil}},
	)

	var calls atomic.Int32

	bodies := make(chan string, 4)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}

		bodies <- string(body)

		_, err = io.WriteString(
			writer,
			`{"choices":[{"message":{"role":"assistant","content":"Actualiza el servicio y su caché."},
"finish_reason":"stop"}]}`,
		)
		if err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()

	profile := config.AIProfile{
		APIKeyEnv:    "",
		Provider:     "openai-compatible",
		Endpoint:     server.URL,
		Model:        "summary-model",
		Timeout:      time.Second,
		MaxDiffBytes: config.DefaultAIDiffBytes,
		Redact:       []redact.Rule{{Pattern: `(?m)^(TOKEN=).*$`, Replacement: `${1}[OCULTO]`}},
	}
	manager := aisummary.New(map[string]config.AIProfile{summaryProfile: profile})

	selection := runner.cfg.Repositories[runner.name]
	selection.IACommit = []string{summaryProfile}
	selection.IAInclude = []string{noise, normal}

	session, err := manager.Session(&selection)
	if err != nil {
		t.Fatal(err)
	}

	runner.ai = session
	writeSource(t, runner, "noise", "TOKEN=old-secret\ncache=changed")
	commitEvent(t, runner, noise, event.Actor{})

	if calls.Load() != 0 {
		t.Fatal("deferred-only change sent to AI")
	}

	excluded := writeSource(t, runner, "private-key", "not-permitted-secret")
	commitEvent(t, runner, excluded, event.Actor{})

	if calls.Load() != 1 {
		t.Fatal("combined commit did not request summary")
	}

	body := <-bodies
	if strings.Contains(body, "old-secret") || strings.Contains(body, "not-permitted-secret") ||
		strings.Contains(body, "private-key") ||
		!strings.Contains(body, "cache=changed") ||
		!strings.Contains(body, "[OCULTO]") {
		t.Fatal("incorrect outbound filtering")
	}

	message := gitOutput(t, runner, "log", "-1", "--format=%B")
	if !strings.Contains(message, "Resumen generado por IA (gpt)") ||
		strings.Count(message, "FSLedger-Change-ID:") != 1 {
		t.Fatal("lost message evidence", message)
	}

	committed := gitOutput(t, runner, "show", "HEAD:"+strings.TrimPrefix(noise, "/"))
	if !strings.Contains(committed, "old-secret") {
		t.Fatal("redaction modified Git content")
	}

	writeSource(t, runner, "noise", "TOKEN=new-secret\ncache=changed")
	commitEvent(t, runner, noise, event.Actor{})
	runner.forceCommit = true

	err = runner.reconcile(t.Context(), "explicit flush")
	if err != nil {
		t.Fatal(err)
	}

	if calls.Load() != 1 || runner.status.AIStatus != "redacted" {
		t.Fatal("hidden-only change requested AI", runner.status.AIStatus)
	}

	runner.stopping = true
	writeSource(t, runner, "normal", "shutdown")
	commitEvent(t, runner, normal, event.Actor{})

	if calls.Load() != 1 {
		t.Fatal("shutdown requested AI")
	}
}

func TestAIFailureStillCommitsAndDoesNotAnnounceError(t *testing.T) {
	t.Parallel()
	runner, _, normal := deferFixture(t, nil)

	server := httptest.NewServer(
		http.HandlerFunc(
			func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusTooManyRequests) },
		),
	)
	defer server.Close()

	manager := aisummary.New(
		map[string]config.AIProfile{
			summaryProfile: {
				APIKeyEnv:    "",
				Provider:     "openai-compatible",
				Endpoint:     server.URL,
				Model:        "summary-model",
				Timeout:      time.Second,
				Redact:       nil,
				MaxDiffBytes: config.DefaultAIDiffBytes,
			},
		},
	)

	selection := runner.cfg.Repositories[runner.name]
	selection.IACommit = []string{summaryProfile}
	selection.IAInclude = []string{normal}

	session, err := manager.Session(&selection)
	if err != nil {
		t.Fatal(err)
	}

	runner.ai = session
	writeSource(t, runner, "normal", "must commit despite AI failure")
	commitEvent(t, runner, normal, event.Actor{})

	if commitCount(t, runner) != "2" || runner.pendingError || runner.status.AIStatus != "unavailable" {
		t.Fatal("AI failure contaminated Git")
	}
}
