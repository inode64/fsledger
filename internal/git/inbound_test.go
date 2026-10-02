package git_test

import (
	"strings"
	"testing"

	gitrepo "github.com/inode64/fsledger/internal/git"
)

func TestFetchConfinesRefsAndDoesNotCheckout(t *testing.T) {
	t.Parallel()
	origin, receiver := newRepository(t), newRepository(t)
	writeRepositoryFile(t, origin, testFile, "remote")
	commitPaths(t, origin, testFile)
	repositoryOutput(t, origin, "branch", "selected")
	repositoryOutput(t, origin, "branch", "unrequested")
	repositoryOutput(t, origin, "tag", "private-tag")
	writeRepositoryFile(t, receiver, testFile, "local")
	commitPaths(t, receiver, testFile)
	before := repositoryOutput(t, receiver, "rev-parse", "HEAD")
	repositoryOutput(t, receiver, "remote", "add", "upstream", origin.Path)
	repositoryOutput(t, receiver, "config", "remote.upstream.fetch", "+refs/*:refs/*")
	repositoryOutput(t, receiver, "config", "remote.upstream.tagOpt", "--tags")
	repositoryOutput(t, receiver, "config", "fetch.prune", "true")
	repositoryOutput(t, receiver, "config", "fetch.pruneTags", "true")
	repositoryOutput(t, receiver, "tag", "local-tag")

	target, err := receiver.FetchBranch(t.Context(), "upstream", "selected")
	if err != nil || target != strings.TrimSpace(repositoryOutput(t, origin, "rev-parse", "HEAD")) {
		t.Fatal("fetch failed", target, err)
	}

	refs := repositoryOutput(t, receiver, "for-each-ref", "--format=%(refname)")
	if strings.Contains(refs, "unrequested") || strings.Contains(refs, "private-tag") ||
		strings.Contains(refs, "heads/selected") || !strings.Contains(refs, "tags/local-tag") {
		t.Fatal("fetch touched implicit refs", refs)
	}

	if repositoryOutput(t, receiver, "rev-parse", "HEAD") != before ||
		repositoryOutput(t, receiver, "show", "HEAD:file") != "local" {
		t.Fatal("fetch changed local history or source")
	}

	missing, err := receiver.FetchBranch(t.Context(), origin.Path, "absent")
	if err != nil || missing != "" {
		t.Fatal("absent branch reused stale incoming ref", missing, err)
	}
}

func TestAdoptIncomingRequiresExactTree(t *testing.T) {
	t.Parallel()
	repository := newRepository(t)
	writeRepositoryFile(t, repository, testFile, "initial")
	commitPaths(t, repository, testFile)
	base := strings.TrimSpace(repositoryOutput(t, repository, "rev-parse", "HEAD"))
	writeRepositoryFile(t, repository, testFile, "target")
	commitPaths(t, repository, testFile)
	target := strings.TrimSpace(repositoryOutput(t, repository, "rev-parse", "HEAD"))
	// Only this private test fixture is reset; production never resets or checks out sources.
	repositoryOutput(t, repository, "reset", "--hard", base)

	err := repository.AdoptIncoming(t.Context(), base, target)
	if err == nil {
		t.Fatal("adopted target without matching source bytes")
	}

	writeRepositoryFile(t, repository, testFile, "target")

	err = repository.AdoptIncoming(t.Context(), base, target)
	if err != nil {
		t.Fatal(err)
	}

	status, err := repository.Status(t.Context())
	if err != nil || status != "" || strings.TrimSpace(repositoryOutput(t, repository, "rev-parse", "HEAD")) != target {
		t.Fatal("adoption changed tree or added a commit", status, err)
	}

	ancestor, err := repository.IsAncestor(t.Context(), base, target)
	if err != nil || !ancestor {
		t.Fatal("failed ancestry check", ancestor, err)
	}

	_, err = repository.ReadBlob(t.Context(), "HEAD:file")
	if err == nil {
		t.Fatal("blob API accepted a mutable expression")
	}
}

func TestIncomingRejectsLargeBlobs(t *testing.T) {
	t.Parallel()
	repository := newRepository(t)
	writeRepositoryFile(t, repository, testFile, strings.Repeat("a", gitrepo.MaxInboundBlob+1))
	commitPaths(t, repository, testFile)
	identifier := strings.TrimSpace(repositoryOutput(t, repository, "rev-parse", "HEAD:file"))

	_, err := repository.ReadBlob(t.Context(), identifier)
	if err == nil {
		t.Fatal("oversized incoming blob accepted")
	}
}
