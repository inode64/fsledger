package notify_test

import (
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/notify"
)

const (
	testChangeEvent = "change"
	testSlackType   = "slack"
)

func TestSlackLongEvidenceIsNotTruncated(t *testing.T) {
	t.Parallel()

	server, texts := slackRecorder(t)
	defer server.Close()

	before := strings.Repeat("A", 20000)
	sender := notify.New(nil)
	message := catalog.Message{
		Host:       "host",
		Repository: "repository",
		Event:      testChangeEvent,
		Count:      1,
		ChangeID:   "change-token",
		Details: []catalog.Detail{
			{
				Path:     []byte("/file"),
				ChangeID: "", Kind: "modified",
				Actor:       "actor",
				Observed:    123,
				Fields:      []catalog.FieldChange{{Field: "hash", Before: before, After: "changed"}},
				BackdatedBy: 0,
			},
		},
	}

	delay, err := sender.Send(
		t.Context(),
		config.Notifier{Type: testSlackType, URL: server.URL, DSN: "", From: "", Template: "", To: nil},
		message, nil,
	)
	if err != nil || delay != 0 {
		t.Fatal(delay, err)
	}

	parts := texts()
	if len(parts) < 2 || !strings.Contains(strings.Join(parts, ""), before) ||
		!strings.Contains(strings.Join(parts, ""), "changed") {
		t.Fatal("notification evidence truncated")
	}
}
