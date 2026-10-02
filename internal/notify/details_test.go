package notify_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/notify"
)

const (
	testChangeEvent = "change"
	testSlackType   = "slack"
)

//nolint:funlen // Local transport records all message parts and checks complete evidence.
func TestSlackLongEvidenceIsNotTruncated(t *testing.T) {
	t.Parallel()

	var (
		mutex sync.Mutex
		parts []string
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

		parts = append(parts, body.Text)
		mutex.Unlock()
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	before := strings.Repeat("A", 20000)
	sender := notify.New(nil)
	message := catalog.Message{
		Host:       "host",
		Repository: "repository",
		Event:      testChangeEvent,
		Count:      1,
		ChangeID:   "approval-token",
		Details: []catalog.Detail{
			{
				Path:     []byte("/file"),
				ChangeID: "", Kind: "modified",
				Actor:    "actor",
				Observed: 123, Baseline: nil,
				Fields: []catalog.FieldChange{{Field: "xattrs", Before: before, After: "changed"}},
			},
		},
	}

	delay, err := sender.Send(
		t.Context(),
		config.Notifier{Type: testSlackType, URL: server.URL, DSN: "", From: "", Template: "", To: nil},
		message,
	)
	if err != nil || delay != 0 {
		t.Fatal(delay, err)
	}

	mutex.Lock()
	defer mutex.Unlock()

	if len(parts) < 2 || !strings.Contains(strings.Join(parts, ""), before) ||
		!strings.Contains(strings.Join(parts, ""), "changed") {
		t.Fatal("notification evidence truncated")
	}
}
