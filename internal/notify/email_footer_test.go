package notify_test

import (
	"io"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/buildinfo"
)

// assertEmailFooter decodes a delivered message and requires the signature after its body.
// The fixture keeps SMTP dot-stuffing, which a real server removes before delivery.
func assertEmailFooter(t *testing.T, data string) {
	t.Helper()

	unstuffed := strings.ReplaceAll(data, "\r\n..", "\r\n.")

	message, err := mail.ReadMessage(strings.NewReader(unstuffed))
	if err != nil {
		t.Fatal(err)
	}

	body := message.Body
	if strings.EqualFold(message.Header.Get("Content-Transfer-Encoding"), "quoted-printable") {
		body = quotedprintable.NewReader(body)
	}

	decoded, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}

	text := strings.ReplaceAll(string(decoded), "\r\n", "\n")
	signature := "\n\n-- \nFSledger | " + buildinfo.Release() +
		"\nChange history, file integrity, and intrusion alerts for your Linux servers." +
		"\nhttps://github.com/inode64/fsledger\n"

	if !strings.HasSuffix(text, signature) {
		t.Fatalf("delivered body lacks the signature:\n%s", text)
	}
}
