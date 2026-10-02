package git_test

import (
	"context"
	"os/exec"
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
