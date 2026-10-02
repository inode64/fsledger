package filter_test

import (
	"os/user"
	"strconv"
	"testing"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/filter"
)

func TestCombinedRules(t *testing.T) {
	const (
		webPath     = "/web/a"
		cronCommand = "/usr/bin/php /web/wp-cron.php"
	)

	t.Parallel()

	rules := []filter.Rule{
		{
			Users:        []string{"uid:123", "uid:456"},
			CommandRegex: []string{`(^|/)php .*artisan schedule:run`, `(^|/)php .*wp-cron\.php`},
			Paths:        nil,
		},
		{Paths: []string{"/data/cache/**"}, Users: nil, CommandRegex: nil},
	}

	matcher, err := filter.Compile(rules)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		path, command          string
		uid                    uint32
		known, userKnown, want bool
	}{
		{webPath, "/usr/bin/php /web/artisan schedule:run", 123, true, true, true},
		{webPath, cronCommand, 456, true, true, true},
		{webPath, "/usr/bin/php /web/index.php", 123, true, true, false},
		{webPath, cronCommand, 789, true, true, false},
		{webPath, "", 123, true, true, false},
		{webPath, cronCommand, 123, false, true, false},
		{webPath, cronCommand, 123, true, false, false},
		{"/data/cache/file", "", 0, false, false, true},
		{"/data/cache-other/file", "", 0, false, false, false},
	} {
		actor := event.Actor{UID: test.uid, Known: test.known, UserKnown: test.userKnown, Command: test.command}
		if got := matcher.Match(test.path, actor); got != test.want {
			t.Fatalf("%+v: matched %v", test, got)
		}
	}
}

func TestNamesAndMalformedRules(t *testing.T) {
	t.Parallel()

	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}

	matcher, err := filter.Compile([]filter.Rule{{Users: []string{account.Username}, Paths: nil, CommandRegex: nil}})
	if err != nil {
		t.Fatal(err)
	}

	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}

	if !matcher.Match("/file", event.Actor{Known: true, UserKnown: true, UID: uint32(uid)}) {
		t.Fatal("name did not resolve")
	}

	for _, rule := range []filter.Rule{
		{},
		{Users: []string{""}, Paths: nil, CommandRegex: nil},
		{Users: []string{"uid:no"}, Paths: nil, CommandRegex: nil},
		{CommandRegex: []string{"("}, Users: nil, Paths: nil},
		{CommandRegex: []string{""}, Users: nil, Paths: nil},
		{Paths: []string{"["}, Users: nil, CommandRegex: nil},
	} {
		_, err = filter.Compile([]filter.Rule{rule})
		if err == nil {
			t.Fatal("invalid rule accepted", rule)
		}
	}
}
