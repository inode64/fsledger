package git_test

import (
	"strings"
	"testing"
)

const rewriteBranch = "main"

func TestFetchFollowsRewrittenRemoteBranch(t *testing.T) {
	t.Parallel()
	origin, receiver := newRepository(t), newRepository(t)
	writeRepositoryFile(t, origin, testFile, "base")
	commitPaths(t, origin, testFile)
	base := strings.TrimSpace(repositoryOutput(t, origin, "rev-parse", "HEAD"))
	writeRepositoryFile(t, origin, testFile, "published")
	commitPaths(t, origin, testFile)

	published, err := receiver.FetchBranch(t.Context(), origin.Path, rewriteBranch)
	if err != nil || published != strings.TrimSpace(repositoryOutput(t, origin, "rev-parse", "HEAD")) {
		t.Fatal("initial fetch failed", published, err)
	}

	// Only this private fixture is reset, emulating a force-pushed rebase of the remote branch.
	repositoryOutput(t, origin, "reset", "--hard", base)
	writeRepositoryFile(t, origin, testFile, "rewritten")
	commitPaths(t, origin, testFile)
	rewritten := strings.TrimSpace(repositoryOutput(t, origin, "rev-parse", "HEAD"))

	target, err := receiver.FetchBranch(t.Context(), origin.Path, rewriteBranch)
	if err != nil || target != rewritten {
		t.Fatal("rewritten remote branch was not fetched", target, err)
	}

	// The private ref follows the remote; ancestry still exposes the rewrite as divergence.
	ancestor, err := receiver.IsAncestor(t.Context(), published, target)
	if err != nil || ancestor {
		t.Fatal("rewritten history reported as fast-forward", ancestor, err)
	}

	rewound, err := receiver.IsAncestor(t.Context(), target, published)
	if err != nil || rewound {
		t.Fatal("rewritten history reported as rewind", rewound, err)
	}
}
