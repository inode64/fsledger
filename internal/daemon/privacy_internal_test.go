package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/filter"
	"github.com/inode64/fsledger/internal/notify"
)

const (
	privateHookOutput = "fixture-private-hook-output"
	privatePassword   = "fixture-private-password"
	privateToken      = "fixture-private-token"
	privateSink       = "privacy-sink"
)

func assertPrivateAbsent(t *testing.T, text string) {
	t.Helper()

	for _, secret := range []string{privateHookOutput, privatePassword, privateToken} {
		if strings.Contains(text, secret) {
			t.Fatal("sensitive fixture data escaped into a persistent or published value")
		}
	}
}

func checkPrivateNotification(t *testing.T, message catalog.Message) {
	t.Helper()

	data, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}

	assertPrivateAbsent(t, string(data))

	received := make(chan string, 1)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Error(readErr)
		}

		received <- string(body)

		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	_, err = notify.New(nil).Send(t.Context(), config.Notifier{
		Type: config.NotifierSlack, URL: server.URL, Template: "", DSN: "", From: "", To: nil,
	}, message)
	if err != nil {
		t.Fatal(err)
	}

	assertPrivateAbsent(t, <-received)
}

func TestGitHookOutputStaysOutOfDiagnostics(t *testing.T) {
	t.Parallel()

	for _, attributed := range []bool{false, true} {
		checkPrivateHookFailure(t, attributed)
	}
}

func checkPrivateHookFailure(t *testing.T, attributed bool) {
	t.Helper()
	runner := makeWorker(t)
	path := writeSource(t, runner, "file", "initial")

	err := runner.reconcile(t.Context(), "initial")
	if err != nil {
		t.Fatal(err)
	}

	runner.catalog.Notifications.Use = []string{privateSink}
	runner.catalog.Notifications.Events = []string{config.NotificationError, config.NotificationRecovery}
	runner.catalog.Notifications.BatchWindow = 0

	var logs bytes.Buffer

	runner.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	hook := filepath.Join(runner.repo.Path, ".git", "hooks", "pre-commit")
	//nolint:gosec // This executable hook and its fictitious secret stay in the private temporary repository.
	err = os.WriteFile(hook, []byte("#!/bin/sh\nprintf '%s\\n' '"+privateHookOutput+
		"'\nprintf '%s\\n' '"+privateHookOutput+"' >&2\nexit 1\n"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	writeSource(t, runner, "file", "changed")

	if attributed {
		runner.accept(event.Raw{Path: path, Time: time.Now(), Actor: event.Actor{PID: 123, StartTime: 1, Known: true}})
	} else {
		runner.dirty = true
	}

	runner.flush(t.Context(), time.Now().Add(time.Second))
	runner.announceError(t.Context())
	runner.saveStatus(t.Context())

	status, err := os.ReadFile(filepath.Join(runner.cfg.Runtime, runner.name+".json"))
	if err != nil {
		t.Fatal(err)
	}

	assertPrivateAbsent(t, string(status))
	assertPrivateAbsent(t, logs.String())

	message := announced(t, runner, config.NotificationError)
	if !strings.Contains(message.Reason, "exit status 1") {
		t.Fatal("Git failure lost its exit status")
	}

	checkPrivateNotification(t, message)

	err = os.Remove(hook)
	if err != nil {
		t.Fatal(err)
	}

	runner.retryAt = time.Time{}
	runner.flush(t.Context(), time.Now().Add(2*time.Second))
	checkPrivateNotification(t, announced(t, runner, config.NotificationRecovery))
	assertPrivateAbsent(t, logs.String())
}

func TestProcessArgumentsStayOutOfCommitAndCatalog(t *testing.T) {
	t.Parallel()

	for _, actor := range []event.Actor{
		{PID: 123, StartTime: 1, Known: true, Executable: "/usr/bin/mysql", Command: "/usr/bin/mysql -p" + privatePassword},
		{
			PID: 456, Known: true, Executable: "/usr/bin/curl", Evidence: "audit:123:1",
			Command: "/usr/bin/curl -H 'Authorization: Bearer " + privateToken + "' https://example.invalid",
		},
	} {
		checkPrivateActor(t, actor)
	}
}

func checkPrivateActor(t *testing.T, actor event.Actor) {
	t.Helper()
	runner := makeWorker(t)
	path := writeSource(t, runner, "file", "initial")

	err := runner.reconcile(t.Context(), "initial")
	if err != nil {
		t.Fatal(err)
	}

	runner.catalog.Notifications.Use = []string{privateSink}
	runner.catalog.Notifications.Events = []string{config.NotificationChange}
	runner.catalog.Notifications.BatchWindow = 0
	settings := runner.cfg.Reports
	settings.Enabled = true
	settings.Use = []string{privateSink}

	err = runner.catalog.ConfigureReports(t.Context(), settings, false)
	if err != nil {
		t.Fatal(err)
	}

	writeSource(t, runner, "file", "changed")
	commitEvent(t, runner, path, actor)
	message := gitOutput(t, runner, "log", "-1", "--format=%B")
	assertPrivateAbsent(t, message)

	if !strings.Contains(message, actor.Executable) || !strings.Contains(message, "pid:") {
		t.Fatal("omitting arguments discarded observed process identity")
	}

	checkPrivateNotification(t, announced(t, runner, config.NotificationChange))

	report, err := runner.catalog.PreviewReport(t.Context())
	if err != nil || report.Report == nil || len(report.Report.Items) == 0 {
		t.Fatal("missing report evidence", err)
	}

	checkPrivateNotification(t, report)

	serialized, err := json.Marshal(actor)
	if err != nil {
		t.Fatal(err)
	}

	assertPrivateAbsent(t, string(serialized))

	matcher, err := filter.Compile([]filter.Rule{
		{CommandRegex: []string{regexp.QuoteMeta(actor.Command)}, Users: nil, Paths: nil},
	})
	if err != nil || !matcher.Match(path, actor) {
		t.Fatal("publication changed in-memory command matching", err)
	}
}

func TestPrivateCommandStillMatchesCommitDeferral(t *testing.T) {
	t.Parallel()
	runner, noise, _ := deferFixture(t, []filter.Rule{
		{CommandRegex: []string{privatePassword}, Users: nil, Paths: nil},
	})
	writeSource(t, runner, "noise", "pending")
	commitEvent(t, runner, noise, event.Actor{
		PID: 123, StartTime: 1, Known: true, Executable: "/usr/bin/mysql", Command: "mysql -p" + privatePassword,
	})

	if commitCount(t, runner) != "1" || len(runner.deferred) != 1 {
		t.Fatal("omitting published arguments disabled command_regex deferral")
	}
}
