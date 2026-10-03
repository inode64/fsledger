package config

import (
	"strings"
	"testing"
)

func TestDuplicateNotifierNamesAreRejected(t *testing.T) {
	t.Parallel()

	cfg, err := Parse(strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}

	cfg.Notifiers = make(map[string]Notifier)
	cfg.Notifiers["sink"] = Notifier{}
	cfg.Notifications.Use = []string{"sink", "sink"}

	err = cfg.validateNotifications()
	if err == nil || !strings.Contains(err.Error(), "duplicate notifier") {
		t.Fatal("duplicated destination accepted", err)
	}
}

func TestMailPortBounds(t *testing.T) {
	t.Parallel()

	for _, port := range []string{"0", "65536", "999999999999999999999999"} {
		_, err := MailURL("smtps://mail.example:" + port)
		if err == nil {
			t.Fatal("accepted invalid port", port)
		}
	}

	for _, port := range []string{"1", "465", "65535"} {
		_, err := MailURL("smtps://mail.example:" + port)
		if err != nil {
			t.Fatal("rejected valid port", port, err)
		}
	}
}
