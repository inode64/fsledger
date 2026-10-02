package notify_test

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/notify"
	"github.com/inode64/fsledger/internal/resource"
)

func TestEmailReportsMissingSTARTTLSWithoutSending(t *testing.T) {
	t.Parallel()

	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(listener)

	finished := make(chan error, 1)
	go func() {
		finished <- smtpWithoutTLS(listener)
	}()

	sender := notify.New(nil)

	_, err = sender.Send(t.Context(), config.Notifier{
		Type: config.NotifierEmail, URL: "", Template: "",
		DSN:  "smtp://private-user:private-password@" + listener.Addr().String() + "?tls=starttls",
		From: "sender@example.invalid", To: []string{"recipient@example.invalid"},
	}, catalog.Message{Repository: "fixture"})
	if err == nil || err.Error() != "email delivery failed: SMTP server does not support required STARTTLS" {
		t.Fatal("missing STARTTLS failure was not safely identified", err)
	}

	serverErr := <-finished
	if serverErr != nil {
		t.Fatal(serverErr)
	}
}

func smtpWithoutTLS(listener net.Listener) error {
	connection, err := listener.Accept()
	if err != nil {
		return fmt.Errorf("accept SMTP fixture: %w", err)
	}
	defer resource.Close(connection)

	err = connection.SetDeadline(time.Now().Add(5 * time.Second))
	if err != nil {
		return fmt.Errorf("set SMTP fixture deadline: %w", err)
	}

	_, err = fmt.Fprint(connection, "220 fixture SMTP\r\n")
	if err != nil {
		return fmt.Errorf("write SMTP greeting: %w", err)
	}

	command, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read SMTP greeting: %w", err)
	}

	if !strings.HasPrefix(command, "EHLO ") {
		return fault.New("expected SMTP EHLO greeting")
	}

	_, err = fmt.Fprint(connection, "250-fixture\r\n250 SIZE 1048576\r\n")
	if err != nil {
		return fmt.Errorf("write SMTP capabilities: %w", err)
	}

	// No AUTH, MAIL, RCPT or DATA is accepted by this fixture.
	return nil
}
