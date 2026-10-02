package aisummary

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/redact"
)

//nolint:gocognit,funlen // One mock lifecycle verifies filtering, redaction, cooldown and immutable evidence.
func TestReportMetadataSelectionAndFallbackRedaction(t *testing.T) {
	t.Parallel()

	var failures atomic.Int32

	first := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		failures.Add(1)
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(first.Close)

	bodies := make(chan string, 2)
	second := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}

		bodies <- string(body)

		writeReply(t, writer, openAIReply)
	}))
	t.Cleanup(second.Close)
	primary, fallback := testProfile(first.URL), testProfile(second.URL)
	primary.Redact = []redact.Rule{{Pattern: "secret\nname", Replacement: "[MASKED]"}}
	fallback.Redact = []redact.Rule{{Pattern: "second-secret", Replacement: "[OTHER]"}}
	manager := New(map[string]config.AIProfile{"primary": primary, "fallback": fallback})
	settings := config.ReportAI{
		Enabled:  true,
		Input:    metadataFixture,
		Profiles: []string{"primary", "fallback"},
		Include:  []string{"/etc/**"},
	}

	session, err := manager.ReportSession(settings)
	if err != nil {
		t.Fatal(err)
	}

	items := []catalog.ReportItem{
		{Detail: catalog.Detail{
			Path: []byte("/etc/secret\nname/second-secret"), Kind: "modified", Actor: "private command",
			Fields:   []catalog.FieldChange{{Field: "xattrs", Before: "private value", After: "another private value"}},
			ChangeID: "", Baseline: nil, Observed: 0,
		}, Head: "", Deferred: true, Violation: false},
		{Detail: catalog.Detail{
			Path: []byte("/excluded/private-path"), Kind: "added", Actor: "", Fields: nil,
			ChangeID: "", Baseline: nil, Observed: 0,
		}, Head: "", Deferred: false, Violation: false},
	}
	for range 2 {
		summary, status := session.SummarizeReport(t.Context(), items)
		if summary == "" || status != "summarized:fallback" {
			t.Fatal(summary, status)
		}

		body := <-bodies
		for _, forbidden := range []string{"secret", "private", "another", "excluded"} {
			if strings.Contains(body, forbidden) {
				t.Fatal("metadata leaked", forbidden, body)
			}
		}

		if !strings.Contains(body, "[MASKED]") || !strings.Contains(body, "[OTHER]") {
			t.Fatal("redaction missing", body)
		}
	}

	if failures.Load() != 1 || string(items[0].Detail.Path) != "/etc/secret\nname/second-secret" {
		t.Fatal("provider not cooled down or evidence mutated")
	}
}

func TestReportEmptySelectionDoesNotCallProvider(t *testing.T) {
	t.Parallel()

	session, err := New(
		nil,
	).ReportSession(config.ReportAI{Profiles: nil, Include: nil, Input: metadataFixture, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	summary, status := session.SummarizeReport(t.Context(), nil)
	if summary != "" || status != "no-selected-metadata" {
		t.Fatal(summary, status)
	}
}
