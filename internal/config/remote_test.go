package config_test

import (
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/config"
)

// Every push writes into a path remote: watched, it would be recorded again after each publication.
func TestLocalRemoteStaysOutsideSourcesAndState(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name, remote, audit string
		valid               bool
	}{
		{"outside", "/srv/backup.git", "", true},
		{"beside state", "/var/lib/fsledger/backup.git", "", true},
		{"remote name", "origin", "", true},
		{"SSH URL", "ssh://git@git.example/backup.git", "", true},
		{"unpublished DB remote", "/srv/backup.git", "/srv/data/a/backup.git", true},
		{"own source", "/srv/www/backup.git", "", false},
		{"unclean own source", "/srv/other/../www/backup.git/", "", false},
		{"containing a source", "/srv", "", false},
		{"other repository pattern", "/srv/data/site/backup.git", "", false},
		{"own worktree", "/var/lib/fsledger/repos/web", "", false},
		{"storage root", "/var/lib/fsledger/repos", "", false},
		{"runtime", "/run/fsledger/backup.git", "", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			base := t.TempDir()
			path := writeConfiguration(t, base, mainFile, "paths: {repositories: [web.yaml, audit.yaml]}")
			writeConfiguration(t, base, "web.yaml", "paths: [/srv/www]\nstorage: {git: {remote: '"+
				testCase.remote+"'}}")

			audit := "type: db\npaths: ['/srv/data/*']"
			if testCase.audit != "" {
				audit += "\nstorage: {git: {remote: '" + testCase.audit + "'}}"
			}

			writeConfiguration(t, base, "audit.yaml", audit)

			_, err := config.Load(path)
			if (err == nil) != testCase.valid {
				t.Fatalf("valid=%v error=%v", testCase.valid, err)
			}

			if err != nil && !strings.Contains(err.Error(), "Git remote") {
				t.Fatalf("rejection does not name the remote: %v", err)
			}
		})
	}
}

// Unrelated histories cannot share a branch: the remote would reject one repository's pushes forever.
func TestRepositoriesPublishToDistinctBranches(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name, second string
		valid        bool
	}{
		{"same branch", "storage: {git: {remote: 'ssh://git@git.example/fleet.git'}}", false},
		{"inherited remote", "", false},
		{"own branch", "storage: {git: {remote: 'ssh://git@git.example/fleet.git', branch: second}}", true},
		{"other remote", "storage: {git: {remote: 'ssh://git@git.example/other.git'}}", true},
		{"remote name", "storage: {git: {remote: origin}}", true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			base := t.TempDir()
			path := writeConfiguration(t, base, mainFile, "paths: {repositories: [first.yaml, second.yaml]}\n"+
				"storage: {git: {remote: 'ssh://git@git.example/fleet.git'}}")
			writeConfiguration(t, base, "first.yaml", "paths: [/srv/first]")
			writeConfiguration(t, base, "second.yaml", "paths: [/srv/second]\n"+testCase.second)

			_, err := config.Load(path)
			if (err == nil) != testCase.valid {
				t.Fatalf("valid=%v error=%v", testCase.valid, err)
			}
		})
	}
}
