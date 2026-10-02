package notify

import (
	"strings"
	"testing"
	"text/template"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
)

func TestIndivisibleDigestSkipsRedundantRendering(t *testing.T) {
	t.Parallel()

	for _, body := range []string{"small", strings.Repeat("x", digestEmailBytes+1), "{{.MissingField}}"} {
		t.Run(body[:min(len(body), 16)], func(t *testing.T) {
			t.Parallel()

			checkIndivisibleDigest(t, body)
		})
	}
}

func checkIndivisibleDigest(t *testing.T, body string) {
	t.Helper()

	renders := 0
	subject := template.Must(template.New("count").Funcs(template.FuncMap{
		"count": func() string {
			renders++

			return "subject"
		},
	}).Parse("{{count}}"))
	sender := New(nil)
	sender.templates[""] = compiledTemplate{subject: subject, body: template.Must(template.New("body").Parse(body))}
	deliveries := []catalog.Delivery{
		{
			ID:          1,
			Destination: schedulingSink,
			Version:     "",
			LastError:   "",
			Attempts:    0,
			NextAttempt: 0,
			Message: catalog.Message{
				Event:  config.NotificationChange,
				Count:  digestMaxPaths,
				Reason: "indivisible evidence",
			},
		},
		{
			ID: 2, Destination: schedulingSink, Version: "", LastError: "", Attempts: 0, NextAttempt: 0,
			Message: catalog.Message{Event: config.NotificationChange, Count: 1, Reason: "deferred evidence"},
		},
	}
	notifier := config.Notifier{Type: config.NotifierEmail, Template: "", DSN: "", From: "", To: nil, URL: ""}

	digest := catalog.MergeMessages(deliveries)[0]
	if len(digest.Deliveries) != 2 {
		t.Fatal("fixture did not merge both deliveries")
	}

	chunk, measured := sender.digestChunk(notifier, digest)
	if measured != nil || renders != 0 || len(chunk.Deliveries) != 1 || chunk.Deliveries[0].ID != 1 ||
		chunk.Message.Count != digestMaxPaths || chunk.Message.Reason != "indivisible evidence" {
		t.Fatal("indivisible delivery was rendered early or changed")
	}

	// An invalid transport prevents delivery; template errors must still be reported first.
	_, err := sender.Send(t.Context(), notifier, chunk.Message)
	if renders != 1 || err == nil {
		t.Fatal("message was rendered repeatedly or its failure was swallowed", renders, err)
	}

	if body == "{{.MissingField}}" && err.Error() != "notification body execution failed" {
		t.Fatal("template error was hidden", err)
	}
}
