package notify

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/resource"
)

const (
	schedulingSink     = "sink"
	schedulingSlack    = "slack"
	schedulingChangeID = "change"
	schedulingPath     = "/path"
)

func TestDueChangesAreDeliveredAsOneDigest(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32

	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

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
	defer resource.Close(store)

	for range 300 {
		err = store.Announce(t.Context(), config.NotificationChange, "")
		if err != nil {
			t.Fatal(err)
		}
	}

	sender := New(nil)
	sender.HTTP = server.Client()

	var logs bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	err = sender.deliverPending(t.Context(), store, destinations(map[string]config.Notifier{
		schedulingSink: {
			Type: schedulingSlack, URL: server.URL, DSN: "", From: "", Template: "", To: nil,
		},
	}), logger)
	if err != nil || requests.Load() != 1 {
		t.Fatalf("requests=%d error=%v", requests.Load(), err)
	}

	stats, err := store.Stats(t.Context())
	if err != nil || stats.PendingNotifications != 0 {
		t.Fatal(stats, err)
	}

	if text := logs.String(); !strings.Contains(text, `"deliveries":300`) ||
		!strings.Contains(text, `"repository":"repo"`) {
		t.Fatal("missing digest log", text)
	}
}

func TestDigestChunkKeepsExcessDeliveriesForAnotherTick(t *testing.T) {
	t.Parallel()

	sender := New(map[string]config.Template{
		"summary": {Subject: "{{.Event}}", Body: "{{.Count}}"},
	})

	deliveries := make([]catalog.Delivery, 2001)

	for index := range deliveries {
		deliveries[index] = catalog.Delivery{
			Destination: schedulingSink, Version: "v1", LastError: "",
			ID: int64(index + 1), Attempts: 0, NextAttempt: 0,
			Message: catalog.Message{
				Repository: "repo", Host: "host", Hostname: "kernel", Event: config.NotificationChange,
				ChangeID: schedulingChangeID, Paths: []string{schedulingPath}, Details: nil, Count: 1,
			},
		}
	}

	digest := catalog.MergeMessages(deliveries)[0]
	chunk, measured := sender.digestChunk(config.Notifier{
		Type: config.NotifierEmail, URL: "", DSN: "", From: "", Template: "summary", To: nil,
	}, digest)

	if measured == nil || !strings.Contains(measured.subject+measured.body, strconv.Itoa(digestMaxPaths)) {
		t.Fatal("accepted digest was not returned already rendered")
	}

	if len(chunk.Deliveries) != digestMaxPaths || chunk.Message.Count != digestMaxPaths {
		t.Fatalf("deliveries=%d paths=%d", len(chunk.Deliveries), chunk.Message.Count)
	}
}

func TestDigestChunkUsesRenderedSizeAndPreservesOversizedEvidence(t *testing.T) {
	t.Parallel()

	large := strings.Repeat("x", 300<<10)
	deliveries := make([]catalog.Delivery, 2)

	for index := range deliveries {
		deliveries[index] = catalog.Delivery{
			Destination: schedulingSink, Version: "v1", LastError: "",
			ID: int64(index + 1), Attempts: 0, NextAttempt: 0,
			Message: catalog.Message{
				Repository: "repo", Host: "host", Hostname: "kernel", Event: config.NotificationChange,
				ChangeID: schedulingChangeID, Paths: []string{schedulingPath}, Count: 1,
				Details: []catalog.Detail{
					{
						ChangeID: schedulingChangeID,
						Kind:     displayModified,
						Actor:    displayUnknown,
						Path:     []byte(schedulingPath),
						Fields:   []catalog.FieldChange{{Field: "hash", Before: large, After: "new"}},
						Baseline: nil,
						Observed: 0,
					},
				},
			},
		}
	}

	sender := New(nil)
	digest := catalog.MergeMessages(deliveries)[0]
	email := config.Notifier{
		Type: config.NotifierEmail, URL: "", DSN: "", From: "", Template: "", To: nil,
	}
	chunk, _ := sender.digestChunk(email, digest)

	if len(chunk.Deliveries) != 1 || chunk.Message.Details[0].Fields[0].Before != large {
		t.Fatal("evidence was split or truncated")
	}

	single, _ := sender.digestChunk(email, catalog.MergeMessages(deliveries[:1])[0])
	if len(single.Message.Details[0].Fields[0].Before) != len(large) {
		t.Fatal("oversized individual evidence was truncated")
	}
}

func TestFailedDigestRetriesEveryDelivery(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32

	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

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
	defer resource.Close(store)

	for range 300 {
		err = store.Announce(t.Context(), config.NotificationChange, "")
		if err != nil {
			t.Fatal(err)
		}
	}

	sender := New(nil)
	sender.HTTP = server.Client()

	err = sender.deliverPending(t.Context(), store, destinations(map[string]config.Notifier{
		schedulingSink: {
			Type: schedulingSlack, URL: server.URL, DSN: "", From: "", Template: "", To: nil,
		},
	}), slog.New(slog.DiscardHandler))
	if err != nil || requests.Load() != 1 {
		t.Fatalf("requests=%d error=%v", requests.Load(), err)
	}

	stats, err := store.Stats(t.Context())
	if err != nil || stats.PendingNotifications != 300 {
		t.Fatal(stats, err)
	}
}

//nolint:funlen // Ordered integration fixture verifies persisted scheduling across a real HTTPS delivery.
func TestLocalRateLimitDoesNotIncreaseAttempts(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32

	server := httptest.NewTLSServer(
		http.HandlerFunc(
			func(writer http.ResponseWriter, _ *http.Request) { requests.Add(1); writer.WriteHeader(http.StatusOK) },
		),
	)
	defer server.Close()

	sender := New(nil)
	sender.HTTP = server.Client()

	cfg, err := config.Load(testConfiguration(t))
	if err != nil {
		t.Fatal(err)
	}

	cfg.Notifications.Use = []string{schedulingSink}
	cfg.Notifications.BatchWindow = 0

	store, err := catalog.Open(
		t.Context(),
		filepath.Join(t.TempDir(), "catalog"),
		"repo",
		"host",
		cfg.Integrity,
		cfg.Notifications,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(store)

	for range 64 {
		err = store.Announce(t.Context(), "error", "")
		if err != nil {
			t.Fatal(err)
		}
	}

	err = sender.deliverPending(
		t.Context(),
		store,
		destinations(
			map[string]config.Notifier{
				schedulingSink: {
					Type: schedulingSlack, URL: server.URL, DSN: "", From: "", Template: "", To: nil,
				},
			},
		),
		slog.New(slog.DiscardHandler),
	)
	if err != nil || requests.Load() != 1 {
		t.Fatal("wrong delivery count", requests.Load(), err)
	}

	timer := time.NewTimer(time.Second + 100*time.Millisecond)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal("test cancelled")
	}

	due, err := store.Due(t.Context())
	if err != nil || len(due) != 63 {
		t.Fatal("local scheduling imposed exponential delay", len(due), err)
	}

	for _, delivery := range due {
		if delivery.Attempts != 0 || delivery.LastError != "" {
			t.Fatal("local slot recorded as failed attempt", delivery)
		}
	}
}

func TestMailDSNDefaultAndExplicitPorts(t *testing.T) {
	t.Parallel()

	for dsn, address := range map[string]string{
		"smtps://mail.example":                "mail.example:465",
		"smtp://mail.example?tls=starttls":    "mail.example:587",
		"smtps://mail.example:2525":           "mail.example:2525",
		"smtp://mail.example:25?tls=starttls": "mail.example:25",
	} {
		client, err := New(
			nil,
		).emailClient(config.Notifier{DSN: dsn, Type: "email", URL: "", From: "", Template: "", To: nil})
		if err != nil || client.ServerAddr() != address {
			t.Fatal("wrong mail endpoint", dsn, client, err)
		}
	}
}
