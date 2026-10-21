package exclude_test

import (
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/exclude"
)

func TestNewNormalizesSeparatorsCleanPathsNeverContain(t *testing.T) {
	t.Parallel()

	for pattern, paths := range map[string]map[string]bool{
		// gitignore's "this directory": the directory itself and everything below it.
		"/etc/ssl/private/": {
			"/etc/ssl/private": true, "/etc/ssl/private/site.key": true, "/etc/ssl/privateer": false, "/etc/ssl": false,
		},
		"**/node_modules/": {"/srv/app/node_modules/x/index.js": true, "/srv/app/node_modules.bak": false},
		"/srv//cache//":    {"/srv/cache/a": true, "/srv/cached": false},
		"//var/tmp":        {"/var/tmp/a": true, "/var/tmpdir": false},
		"cache/":           {"/srv/cache/a": false, "cache/a": true},
		// An escaped separator stays as written: still accepted, as before.
		`/srv\/`: {"/srv": false},
	} {
		matcher, err := exclude.New(pattern)
		if err != nil {
			t.Fatalf("%q rejected: %v", pattern, err)
		}

		for path, excluded := range paths {
			if matcher.Match(path) != excluded {
				t.Errorf("%q on %q: excluded=%v, want %v", pattern, path, !excluded, excluded)
			}
		}
	}
}

func TestSelectionNamesDirectoryContents(t *testing.T) {
	t.Parallel()

	for pattern, want := range map[string]string{
		// Only a trailing slash names a directory whose contents an allowlist selects.
		"/etc/":       "/etc/**",
		"//etc//app/": "/etc/app/**",
		"**/conf.d/":  "**/conf.d/**",
		"/":           "/**",
		// Other patterns only collapse "//"; an escaped separator names no directory.
		"/etc":         "/etc",
		"/etc/*.conf":  "/etc/*.conf",
		"/srv//a.conf": "/srv/a.conf",
		`/etc\/`:       `/etc\/`,
	} {
		got, err := exclude.Selection(pattern)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", pattern, got, err, want)
		}
	}

	for _, pattern := range []string{"/etc/../shadow", "/etc/./", "", "/etc/["} {
		_, err := exclude.Selection(pattern)
		if err == nil {
			t.Errorf("%q accepted", pattern)
		}
	}
}

func TestNewRejectsDotComponents(t *testing.T) {
	t.Parallel()

	for _, pattern := range []string{"/srv/../etc/shadow", "/etc/./ssl/private/**", "./etc", "**/..", "/.", "."} {
		_, err := exclude.New("**/*.log", pattern)
		if err == nil || !strings.Contains(err.Error(), `invalid exclusion "`+pattern+`"`) ||
			!strings.Contains(err.Error(), `".."`) {
			t.Errorf("%q: got %v", pattern, err)
		}
	}

	// Dots inside a component are ordinary names and globs.
	_, err := exclude.New("**/.env", "**/..data/**", "/etc/.*", "**/*.")
	if err != nil {
		t.Fatal(err)
	}
}
