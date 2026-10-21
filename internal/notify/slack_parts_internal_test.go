package notify

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/resource"
)

const (
	partsTemplate = "parts"
	partsTail     = "tail"
	slowWebhook   = 300 * time.Millisecond
	probeInterval = 10 * time.Millisecond
)

// partsFixture renders an error as exactly one full Slack part followed by partsTail.
func partsFixture(t *testing.T) (map[string]config.Template, string, string) {
	t.Helper()

	head := config.NotificationError + "\n"
	first := head + strings.Repeat("a", slackPartSize-len(head))

	return map[string]config.Template{partsTemplate: parsedTemplate(t, "{{.Event}}", "{{.Reason}}")},
		first[len(head):] + partsTail, first
}

// partsWebhook records the text of every accepted post; respond may answer a request itself.
func partsWebhook(
	t *testing.T,
	respond func(writer http.ResponseWriter, request int) bool,
) (*httptest.Server, func() []string) {
	t.Helper()

	var (
		mutex    sync.Mutex
		requests int
		accepted []string
	)

	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Text string `json:"text"`
		}

		err := json.NewDecoder(request.Body).Decode(&payload)
		if err != nil {
			t.Error(err)
		}

		mutex.Lock()
		requests++
		current := requests
		mutex.Unlock()

		if respond(writer, current) {
			return
		}

		mutex.Lock()

		accepted = append(accepted, payload.Text)

		mutex.Unlock()
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	return server, func() []string {
		mutex.Lock()
		defer mutex.Unlock()

		return slices.Clone(accepted)
	}
}

// partsOutbox queues one error carrying reason for the Slack destination schedulingSink.
func partsOutbox(t *testing.T, reason string) *catalog.Store {
	t.Helper()

	cfg, err := config.Load(testConfiguration(t))
	if err != nil {
		t.Fatal(err)
	}

	cfg.Notifications.Use = []string{schedulingSink}
	cfg.Notifications.BatchWindow = 0

	store, err := catalog.Open(
		t.Context(), filepath.Join(t.TempDir(), "catalog"), "repo", "host", cfg.Integrity, cfg.Notifications,
	)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.Close(store) })

	err = store.Announce(t.Context(), config.NotificationError, reason)
	if err != nil {
		t.Fatal(err)
	}

	return store
}

// A part that fails after earlier parts were accepted must not post those parts again, even when the
// retry runs in a restarted sender: the outbox records each accepted prefix.
func TestSlackRetryResumesAfterAcceptedParts(t *testing.T) {
	t.Parallel()

	templates, reason, first := partsFixture(t)
	ctx, cancel := context.WithCancel(t.Context())

	server, accepted := partsWebhook(t, func(writer http.ResponseWriter, request int) bool {
		if request != 2 {
			return false
		}

		// The daemon stops while the second part is in flight, so that part is never acknowledged.
		cancel()
		writer.WriteHeader(http.StatusServiceUnavailable)

		return true
	})

	store := partsOutbox(t, reason)
	routing := destinations(map[string]config.Notifier{
		schedulingSink: {
			Type: schedulingSlack, URL: server.URL, DSN: "", From: "", Template: partsTemplate, To: nil,
		},
	})

	deliver := func(attempt context.Context) error {
		sender := New(templates)
		sender.HTTP = server.Client()

		return sender.deliverPending(attempt, store, routing, slog.New(slog.DiscardHandler))
	}

	err := deliver(ctx)
	if err == nil {
		t.Fatal("interrupted delivery was acknowledged")
	}

	err = deliver(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if posted := accepted(); !slices.Equal(posted, []string{first, partsTail}) {
		t.Fatalf("posted %d parts, want the first part once and then the tail", len(posted))
	}

	stats, err := store.Stats(t.Context())
	if err != nil || stats.PendingNotifications != 0 {
		t.Fatal(stats, err)
	}
}

// Between two parts of one message the webhook stays reserved, so another message to it waits for the
// last part instead of landing in between, even when the webhook answers slowly.
func TestSlackPartsKeepTheWebhookReserved(t *testing.T) {
	t.Parallel()

	templates, reason, first := partsFixture(t)
	started := make(chan struct{})

	server, accepted := partsWebhook(t, func(_ http.ResponseWriter, request int) bool {
		if request == 1 {
			close(started)
			time.Sleep(slowWebhook)
		}

		return false
	})

	sender := New(templates)
	sender.HTTP = server.Client()
	notifier := config.Notifier{
		Type: schedulingSlack, URL: server.URL, DSN: "", From: "", Template: partsTemplate, To: nil,
	}
	probe := make(chan error, 1)

	go func() {
		select {
		case <-started:
		case <-t.Context().Done():
			probe <- t.Context().Err()

			return
		}

		for {
			_, err := sender.Send(t.Context(), notifier, catalog.Message{Event: config.NotificationRecovery})
			if !errors.Is(err, errLocalRateLimit) {
				probe <- err

				return
			}

			time.Sleep(probeInterval)
		}
	}()

	_, err := sender.Send(t.Context(), notifier, catalog.Message{Event: config.NotificationError, Reason: reason})
	if err != nil {
		t.Fatal(err)
	}

	err = <-probe
	if err != nil {
		t.Fatal(err)
	}

	want := []string{first, partsTail, config.NotificationRecovery + "\n"}
	if posted := accepted(); !slices.Equal(posted, want) {
		t.Fatalf("posted %q, want both parts before the other message", headings(posted))
	}
}

func headings(texts []string) []string {
	const length = 8

	result := make([]string, len(texts))
	for index, text := range texts {
		result[index] = text[:min(length, len(text))]
	}

	return result
}
