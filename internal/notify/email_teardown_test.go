package notify_test

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/notify"
	"github.com/inode64/fsledger/internal/resource"
)

// TestEmailAcceptedDataSurvivesTeardownFailure keeps a relay that drops the
// session after accepting DATA from turning one notification into endless
// retries, while a session dropped before DATA is accepted still fails.
func TestEmailAcceptedDataSurvivesTeardownFailure(t *testing.T) {
	t.Parallel()

	for dropped, accepted := range map[string]int{"QUIT": 1, "RSET": 1, "DATA": 0} {
		t.Run(dropped, func(t *testing.T) {
			t.Parallel()

			listener, pool := teardownListener(t)

			delivered := make(chan int, 1)
			failure := make(chan error, 1)

			go func() {
				count, serveErr := serveTeardownSMTP(listener, dropped)
				delivered <- count

				failure <- serveErr
			}()

			sender := notify.New(nil)
			//nolint:exhaustruct_v5 // Only trust roots and the TLS minimum differ from secure library defaults.
			sender.TLS = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

			_, err := sender.Send(t.Context(), config.Notifier{
				Type: config.NotifierEmail, URL: "", Template: "",
				DSN:  "smtps://" + listener.Addr().String(),
				From: "teardown-sender@example.invalid", To: []string{"teardown-recipient@example.invalid"},
			}, catalog.Message{Repository: "teardown", Event: testChangeEvent, Count: 1}, nil)
			if (err == nil) != (accepted == 1) {
				t.Fatal("delivery result does not follow the DATA reply", err)
			}

			if count := <-delivered; count != accepted {
				t.Fatal("unexpected accepted message count", count)
			}

			serveErr := <-failure
			if serveErr != nil {
				t.Fatal(serveErr)
			}
		})
	}
}

func teardownListener(t *testing.T) (net.Listener, *x509.CertPool) {
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

	t.Cleanup(func() { resource.Close(secure) })

	return secure, pool
}

// serveTeardownSMTP accepts DATA with 250 and closes the connection without a
// reply when the client sends the dropped command.
func serveTeardownSMTP(listener net.Listener, dropped string) (int, error) {
	connection, err := listener.Accept()
	if err != nil {
		return 0, fmt.Errorf("accept SMTP fixture: %w", err)
	}
	defer resource.Close(connection)

	err = connection.SetDeadline(time.Now().Add(5 * time.Second))
	if err != nil {
		return 0, fmt.Errorf("set SMTP fixture deadline: %w", err)
	}

	reader := bufio.NewReader(connection)
	accepted := 0
	response := "220 teardown SMTP\r\n"

	for {
		_, err = io.WriteString(connection, response)
		if err != nil {
			return accepted, fmt.Errorf("write SMTP fixture: %w", err)
		}

		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			return accepted, fmt.Errorf("read SMTP command: %w", readErr)
		}

		command := strings.ToUpper(strings.TrimSpace(line))

		switch {
		case command == dropped:
			return accepted, nil
		case command == "QUIT":
			_, err = io.WriteString(connection, "221 teardown bye\r\n")

			return accepted, fault.Wrap("write SMTP goodbye", err)
		case strings.HasPrefix(command, "EHLO"):
			response = "250-teardown\r\n250 8BITMIME\r\n"
		case command == "DATA":
			err = readTeardownData(connection, reader)
			if err != nil {
				return accepted, err
			}

			accepted++
			response = "250 teardown queued\r\n"
		default:
			response = "250 teardown OK\r\n"
		}
	}
}

func readTeardownData(connection io.Writer, reader *bufio.Reader) error {
	_, err := io.WriteString(connection, "354 teardown data\r\n")
	if err != nil {
		return fmt.Errorf("write SMTP data prompt: %w", err)
	}

	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			return fmt.Errorf("read SMTP data: %w", readErr)
		}

		if line == ".\r\n" {
			return nil
		}
	}
}
