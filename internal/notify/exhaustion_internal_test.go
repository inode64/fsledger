package notify

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
)

// A revoked webhook must not be retried forever. The failure that exhausts the delivery is logged once and
// later ticks send nothing, but the evidence stays queued and a restart sends it once the webhook works.
func TestExhaustedDeliveryIsLoggedOnceAndRetriedAfterRestart(t *testing.T) {
	t.Parallel()

	var requests, status atomic.Int32

	status.Store(http.StatusNotFound)

	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.WriteHeader(int(status.Load()))
	}))
	defer server.Close()

	notifiers := map[string]config.Notifier{schedulingSink: sinkNotifier(server.URL, "")}
	store := exhaustedOutbox(t, notifiers[schedulingSink])

	var logs bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	sender := New(nil)
	sender.HTTP = server.Client()

	for range 2 {
		err := sender.deliverPending(t.Context(), store, destinations(notifiers), logger)
		if err != nil {
			t.Fatal(err)
		}
	}

	if requests.Load() != 1 || strings.Count(logs.String(), "notification retries exhausted") != 1 {
		t.Fatalf("requests=%d logs=%s", requests.Load(), logs.String())
	}

	assertOutbox(t, store, 1, 1)

	status.Store(http.StatusOK)
	logs.Reset()
	store.BindDestinations(notifiers)

	restarted := New(nil)
	restarted.HTTP = server.Client()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		restarted.Run(ctx, store, notifiers, logger)
	}()

	waitForOutbox(t, store)
	cancel()
	<-done

	if requests.Load() != 2 || !strings.Contains(logs.String(), "exhausted notifications retried") {
		t.Fatalf("requests=%d logs=%s", requests.Load(), logs.String())
	}

	assertOutbox(t, store, 0, 0)
}

// exhaustedOutbox queues one error whose next failure is its last attempt, already due.
func exhaustedOutbox(t *testing.T, notifier config.Notifier) *catalog.Store {
	t.Helper()

	store := partsOutbox(t, "", notifier)

	due, err := store.Due(t.Context())
	if err != nil || len(due) != 1 {
		t.Fatalf("due=%d error=%v", len(due), err)
	}

	for range catalog.MaxDeliveryAttempts - 1 {
		_, err = store.RetryMany(t.Context(), []int64{due[0].ID}, time.Millisecond, "slack HTTP status 404")
		if err != nil {
			t.Fatal(err)
		}
	}

	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal("test cancelled")
	}

	return store
}

func assertOutbox(t *testing.T, store *catalog.Store, pending, exhausted int64) {
	t.Helper()

	stats, err := store.Stats(t.Context())
	if err != nil || stats.PendingNotifications != pending || stats.ExhaustedNotifications != exhausted {
		t.Fatalf("stats=%+v error=%v; want pending=%d exhausted=%d", stats, err, pending, exhausted)
	}
}

// waitForOutbox waits for the sender ticks to empty the outbox.
func waitForOutbox(t *testing.T, store *catalog.Store) {
	t.Helper()

	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		stats, err := store.Stats(t.Context())
		if err != nil || stats.PendingNotifications == 0 {
			return
		}

		select {
		case <-deadline.C:
			t.Fatal("restart did not send the exhausted delivery")
		case <-ticker.C:
		}
	}
}

// cancelAfterSend accepts every request, then stops the daemon before the sender confirms it.
type cancelAfterSend struct{ cancel context.CancelFunc }

func (transport cancelAfterSend) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.cancel()

	//nolint:exhaustruct_v5 // The sender reads only the status, headers and body.
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{}, Request: request,
		Body: io.NopCloser(strings.NewReader("ok")),
	}, nil
}

// A delivery accepted while the daemon stops must be confirmed, or the restart sends it again.
func TestDeliveryAcceptedDuringStopIsNotSentAgain(t *testing.T) {
	t.Parallel()

	notifiers := map[string]config.Notifier{schedulingSink: sinkNotifier("https://hooks.example/sink", "")}
	store := partsOutbox(t, "", notifiers[schedulingSink])
	ctx, cancel := context.WithCancel(t.Context())

	sender := New(nil)
	sender.HTTP.Transport = cancelAfterSend{cancel: cancel}

	err := sender.deliverPending(ctx, store, destinations(notifiers), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	assertOutbox(t, store, 0, 0)
}
