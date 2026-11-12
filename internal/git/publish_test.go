package git_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	gitrepo "github.com/inode64/fsledger/internal/git"
)

func TestPublishIsolatesBranchesAndNeverForces(t *testing.T) {
	t.Parallel()
	remote := &gitrepo.Repository{Path: t.TempDir(), Host: testHost, Timeout: 0}

	//nolint:gosec // Git receives explicit arguments for this test's private temporary repository.
	_, err := exec.CommandContext(t.Context(), "git", "init", "--bare", remote.Path).Output()
	if err != nil {
		t.Fatal(err)
	}

	first, second := newRepository(t), newRepository(t)
	for _, repository := range []*gitrepo.Repository{first, second} {
		writeRepositoryFile(t, repository, "etc/config", repository.Path)
		commitPaths(t, repository, "etc/config")
	}

	firstID, err := first.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	secondID, err := second.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Even administrator push defaults must not publish extra branches or tags.
	repositoryOutput(t, first, "remote", "add", "backup", remote.Path)
	repositoryOutput(t, first, "config", "remote.backup.mirror", "true")
	repositoryOutput(t, first, "config", "push.followTags", "true")
	repositoryOutput(t, first, "tag", "private-tag")

	err = first.Publish(t.Context(), "backup", "web01", firstID)
	if err != nil {
		t.Fatal(err)
	}

	err = second.Publish(t.Context(), remote.Path, "web02", secondID)
	if err != nil {
		t.Fatal(err)
	}

	refs := strings.Fields(repositoryOutput(t, remote, "for-each-ref", "--format=%(refname)"))
	if len(refs) != 2 || refs[0] != "refs/heads/web01" || refs[1] != "refs/heads/web02" {
		t.Fatal("publication changed unrequested refs", refs)
	}

	err = second.Publish(t.Context(), remote.Path, "web01", secondID)
	if err == nil {
		t.Fatal("divergent server overwrote another history")
	}

	actual := strings.TrimSpace(repositoryOutput(t, remote, "rev-parse", "refs/heads/web01"))
	if actual != firstID {
		t.Fatal("rejected push modified original branch")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err = first.Publish(ctx, remote.Path, "web01", firstID)
	if err == nil {
		t.Fatal("cancelled publication succeeded")
	}
}

// A remote path or URL may hold an equals sign, where Git ends the key of a -c argument. The setting that
// keeps a mirror remote from publishing every ref must reach Git all the same.
func TestPublishToRemoteWhosePathHoldsAnEqualsSign(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "host=web01", "repo.git")
	remote := &gitrepo.Repository{Path: path, Host: testHost, Timeout: 0}

	err := os.MkdirAll(remote.Path, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	//nolint:gosec // Git receives explicit arguments for this test's private temporary repository.
	_, err = exec.CommandContext(t.Context(), "git", "init", "--bare", remote.Path).Output()
	if err != nil {
		t.Fatal(err)
	}

	repository := newRepository(t)
	writeRepositoryFile(t, repository, "etc/config", "one")
	commitPaths(t, repository, "etc/config")

	head, err := repository.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	repositoryOutput(t, repository, "config", "remote."+remote.Path+".mirror", "true")
	repositoryOutput(t, repository, "tag", "private-tag")

	err = repository.Publish(t.Context(), remote.Path, "web01", head)
	if err != nil {
		t.Fatal(err)
	}

	refs := strings.Fields(repositoryOutput(t, remote, "for-each-ref", "--format=%(refname)"))
	if !slices.Equal(refs, []string{"refs/heads/web01"}) {
		t.Fatal("publication changed unrequested refs", refs)
	}
}
