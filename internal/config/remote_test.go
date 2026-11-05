package config_test

import (
	"os"
	"path/filepath"
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

// Git publishes through a trusted link. The lexical path can sit outside state and sources while the
// target is reserved, or two links can name one branch.
func TestTrustedRemoteLinkIsThePublishedPath(t *testing.T) {
	t.Parallel()

	tree := t.TempDir()
	state := filepath.Join(tree, "state")
	repo := filepath.Join(tree, "repo.git")

	for _, dir := range []string{filepath.Join(state, "web"), repo} {
		err := os.MkdirAll(dir, 0o700)
		if err != nil {
			t.Fatal(err)
		}
	}

	intoState := filepath.Join(tree, "into-state.git")
	alias := filepath.Join(tree, "alias.git")

	for _, link := range []struct{ name, target string }{
		{intoState, filepath.Join(state, "web")},
		{alias, repo},
	} {
		err := os.Symlink(link.target, link.name)
		if err != nil {
			t.Fatal(err)
		}
	}

	err := loadPathRemote(t, tree, state, intoState)
	if err == nil || !strings.Contains(err.Error(), "reserved state") {
		t.Fatal("trusted link into state was accepted", err)
	}

	err = loadPathRemote(t, tree, state, filepath.Join(tree, "missing.git"))
	if err != nil {
		t.Fatal("missing remote was rejected before publication", err)
	}

	err = loadSharedBranch(t, tree, state, repo, alias)
	if err == nil || !strings.Contains(err.Error(), "same Git branch") {
		t.Fatal("links to one repository were accepted", err)
	}
}

func loadPathRemote(t *testing.T, tree, state, remote string) error {
	t.Helper()

	path := writeConfiguration(t, tree, mainFile, "paths: {repositories: [web.yaml]}\nstorage: {path: '"+
		state+"'}")
	writeConfiguration(t, tree, "web.yaml", "paths: ['"+filepath.Join(tree, "www")+"']\nstorage: {git: {remote: '"+
		remote+"'}}")

	_, err := config.Load(path)

	return err
}

func loadSharedBranch(t *testing.T, tree, state, direct, link string) error {
	t.Helper()

	path := writeConfiguration(t, tree, mainFile, "paths: {repositories: [one.yaml, two.yaml]}\nstorage: {path: '"+
		state+"'}")
	writeConfiguration(t, tree, "one.yaml", "paths: ['"+filepath.Join(tree, "www")+"']\nstorage: {git: {remote: '"+
		direct+"'}}")
	writeConfiguration(t, tree, "two.yaml", "paths: ['"+filepath.Join(tree, "other")+"']\nstorage: {git: {remote: '"+
		link+"'}}")

	_, err := config.Load(path)

	return err
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
