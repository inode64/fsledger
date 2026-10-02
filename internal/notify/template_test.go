package notify_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/notify"
)

// Custom templates can summarize a message without iterating over its details.
func TestTemplateSummaryVariables(t *testing.T) {
	t.Parallel()

	server, rendered := slackRecorder(t)
	defer server.Close()

	sender := notify.New(map[string]config.Template{"summary": {
		Subject: "{{.Host}} {{.Hostname}}",
		Body:    "added={{.Added}} modified={{.Modified}} deleted={{.Deleted}}",
	}})
	detail := func(kind string) catalog.Detail {
		return catalog.Detail{
			ChangeID: "", Path: []byte("/file"), Kind: kind, Actor: "actor",
			Fields: nil, Baseline: nil, Observed: 1,
		}
	}
	message := catalog.Message{
		Host:       "web01",
		Hostname:   "web01.example.org",
		Repository: "repository",
		Event:      testChangeEvent,
		ChangeID:   "change",
		Paths:      nil,
		Count:      4,
		Details:    []catalog.Detail{detail("added"), detail("modified"), detail("modified"), detail("deleted")},
	}

	delay, err := sender.Send(
		t.Context(),
		config.Notifier{Type: testSlackType, URL: server.URL, DSN: "", From: "", Template: "summary", To: nil},
		message,
	)
	if err != nil || delay != 0 {
		t.Fatal(delay, err)
	}

	want := "web01 web01.example.org\nadded=1 modified=2 deleted=1"
	if texts := rendered(); len(texts) != 1 || texts[0] != want {
		t.Fatalf("rendered %q; want %q", texts, want)
	}
}

// slackRecorder captures every Slack text part received by a local webhook.
func slackRecorder(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()

	var (
		mutex sync.Mutex
		texts []string
	)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Text string `json:"text"`
		}

		err := json.NewDecoder(request.Body).Decode(&body)
		if err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)

			return
		}

		mutex.Lock()

		texts = append(texts, body.Text)
		mutex.Unlock()
		writer.WriteHeader(http.StatusOK)
	}))

	return server, func() []string {
		mutex.Lock()
		defer mutex.Unlock()

		return slices.Clone(texts)
	}
}
