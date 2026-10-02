package notify_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/resource"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/notify"
)

const transportTemplate = "transport"

func TestSlackTransportAndRateLimit(t *testing.T) {
	t.Parallel()

	received := make(chan string, 1)

	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Text string `json:"text"`
		}

		err := json.NewDecoder(request.Body).Decode(&payload)
		if err != nil {
			t.Error(err)
		}

		received <- payload.Text

		writer.Header().Set("Retry-After", "2")
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	sender := notify.New(map[string]config.Template{
		transportTemplate: {Subject: "Subject {{.Repository}}", Body: "Body {{.Repository}}"},
	})
	sender.HTTP = server.Client()

	delay, err := sender.Send(
		t.Context(),
		config.Notifier{
			Type: testSlackType, URL: server.URL + "/secret", DSN: "", From: "", To: nil, Template: transportTemplate,
		},
		catalog.Message{Repository: "repo", Event: testChangeEvent, Count: 1},
	)
	if err == nil || delay != 2*time.Second || strings.Contains(err.Error(), "secret") {
		t.Fatalf("%v %v", delay, err)
	}

	if body := <-received; body != "Subject repo\nBody repo" {
		t.Fatal(body)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = sender.Send(
		ctx,
		config.Notifier{
			Type: testSlackType, URL: server.URL + "/secret", DSN: "", From: "", To: nil, Template: transportTemplate,
		},
		catalog.Message{},
	)
	if err == nil {
		t.Fatal("cancelled rate limiter sent")
	}
}

func TestEmailImplicitTLS(t *testing.T) {
	t.Parallel()

	for _, serverName := range []string{"", "example.com", "wrong.invalid"} {
		t.Run("certificate-name="+serverName, func(t *testing.T) {
			t.Parallel()
			testEmailImplicitTLS(t, serverName, false)
		})
	}
}

//nolint:cyclop,funlen,gocognit,gocyclo // Ordered integration fixture keeps setup, transitions and assertions together.
func testEmailImplicitTLS(t *testing.T, serverName string, report bool) {
	t.Helper()

	certificateServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	tlsConfig := certificateServer.TLS.Clone()
	pool := x509.NewCertPool()
	pool.AddCert(certificateServer.Certificate())
	certificateServer.Close()

	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	secure := tls.NewListener(listener, tlsConfig)
	defer resource.Close(secure)

	received := make(chan string, 1)
	failure := make(chan error, 1)

	go func() {
		connection, acceptErr := secure.Accept()
		if acceptErr != nil {
			failure <- acceptErr

			return
		}
		defer resource.Close(connection)

		deadlineErr := connection.SetDeadline(time.Now().Add(5 * time.Second))
		if deadlineErr != nil {
			failure <- deadlineErr

			return
		}

		reader := bufio.NewReader(connection)

		write := func(text string) error {
			_, writeErr := fmt.Fprint(connection, text)

			return fault.Wrap("write SMTP fixture", writeErr)
		}

		writeErr := write("220 localhost SMTP\r\n")
		if writeErr != nil {
			failure <- writeErr

			return
		}

		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				failure <- readErr

				return
			}

			response := "250 OK\r\n"

			switch {
			case strings.HasPrefix(line, "EHLO"):
				response = "250-localhost\r\n250 AUTH PLAIN\r\n"
			case strings.HasPrefix(line, "DATA"):
				writeErr := write("354 send data\r\n")
				if writeErr != nil {
					failure <- writeErr

					return
				}

				var body strings.Builder

				for {
					part, dataErr := reader.ReadString('\n')
					if dataErr != nil {
						failure <- dataErr

						return
					}

					if part == ".\r\n" {
						break
					}

					_, writeErr = body.WriteString(part)
					if writeErr != nil {
						failure <- writeErr

						return
					}
				}

				received <- body.String()
			case strings.HasPrefix(line, "QUIT"):
				writeErr := write("221 bye\r\n")
				if writeErr != nil {
					failure <- writeErr
				}

				return
			}

			writeErr := write(response)
			if writeErr != nil {
				failure <- writeErr

				return
			}
		}
	}()

	sender := notify.New(map[string]config.Template{
		"mail": {Subject: "Subject {{.Repository}}", Body: "Body {{.Repository}}"},
	})
	//nolint:exhaustruct_v5 // Only trust roots and the TLS minimum differ from secure library defaults.
	sender.TLS = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	notifier := config.Notifier{
		Type: "email", URL: "", Template: "mail",
		DSN:  "smtps://" + listener.Addr().String(),
		From: "sender@example.org",
		To:   []string{"receiver@example.org"},
	}

	if serverName != "" {
		notifier.DSN += "?tls_server_name=" + serverName
	}

	message := catalog.Message{Repository: "fixture", Event: testChangeEvent, Count: 1}
	if report {
		message.Event = catalog.ReportEvent
		message.ChangeID = "stable-part"
		message.Report = &catalog.ReportInfo{
			Aggregate: nil,
			ID:        "stable", Summary: "ReportTest", AIStatus: "disabled",
			Items: nil, From: 1, Until: 2, Part: 1, More: false, CoverageGap: false,
		}
	}

	_, err = sender.Send(
		t.Context(),
		notifier,
		message,
	)
	if sender.TLS.ServerName != "" {
		t.Fatal("shared TLS configuration was mutated")
	}

	if serverName == "wrong.invalid" {
		if err == nil || !strings.Contains(err.Error(), "TLS certificate hostname mismatch") ||
			strings.Contains(err.Error(), serverName) {
			t.Fatal("certificate error missing or unsanitized", err)
		}

		return
	}

	if err != nil {
		t.Fatal(err)
	}

	select {
	case body := <-received:
		if report && (!strings.Contains(body, "ReportTest") || !strings.Contains(body, "@fsledger.invalid>")) {
			t.Fatal("report identity or body missing", body)
		}

		if !report && (!strings.Contains(body, "Subject: Subject fixture") || !strings.Contains(body, "Body fixture")) {
			t.Fatal(body)
		}
	case err = <-failure:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("no mail received")
	}
}

func TestReportEmailImplicitTLS(t *testing.T) {
	t.Parallel()
	testEmailImplicitTLS(t, "", true)
}
