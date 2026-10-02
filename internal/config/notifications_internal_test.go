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
