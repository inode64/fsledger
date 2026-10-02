package exclude_test

import (
	"testing"

	"github.com/inode64/fsledger/internal/exclude"
)

func TestMatchCoversDirectoriesAncestorsAndProtectedTrees(t *testing.T) {
	t.Parallel()

	matcher, err := exclude.New(
		"/srv/site/**/cache/**", "**/*.log", "/etc/*.backup.upd-*/**", "**/old", "/etc/ha/deps*/**", "**/tmpdir*/**",
	)
	if err != nil {
		t.Fatal(err)
	}

	matcher = matcher.Protect("/var/lib/state[1]")

	for path, excluded := range map[string]bool{
		"/srv/site/cache":                        true, // the directory itself, not only its contents
		"/srv/site/a/b/cache/file.php":           true,
		"/srv/site/cached/file.php":              false,
		"/srv/site/a/debug.log":                  true,
		"/etc/application.backup.upd-1":          true,
		"/etc/application.backup.upd-1/services": true,
		"/etc/application.conf":                  false,
		"/home/user/old/deep/file":               true, // an excluded ancestor excludes everything below
		// A trailing wildcard that matches nothing more must still exclude the directory itself:
		// doublestar alone does not match "deps*/**" against "deps".
		"/etc/ha/deps":              true,
		"/etc/ha/deps.old/x":        true,
		"/srv/tmpdir":               true,
		"/etc/ha/dep":               false,
		"/home/user/older":          false,
		"/var/lib/state[1]/catalog": true, // protected trees are literal, not patterns
		"/var/lib/state1/catalog":   false,
		"/srv/repo/.git/config":     true,
	} {
		if matcher.Match(path) != excluded {
			t.Errorf("%s: excluded=%v, want %v", path, !excluded, excluded)
		}
	}

	_, err = exclude.New("/etc/[a")
	if err == nil {
		t.Fatal("invalid pattern accepted")
	}
}
