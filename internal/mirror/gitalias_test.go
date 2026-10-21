package mirror_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/exclude"
	gitrepo "github.com/inode64/fsledger/internal/git"
	"github.com/inode64/fsledger/internal/mirror"
	"github.com/inode64/fsledger/internal/resource"
)

func gitMirror(t *testing.T, source string) (*gitrepo.Repository, *mirror.Sync) {
	t.Helper()

	_, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}

	repository := &gitrepo.Repository{Path: t.TempDir(), Host: "testhost", Timeout: 0}
	_, err = repository.Init(t.Context())
	must(t, err)

	matcher, err := exclude.New()
	must(t, err)

	syncer, err := mirror.Open(repository.Path, []string{source}, matcher, "")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.Close(syncer) })

	return repository, syncer
}

func mirrorRelative(t *testing.T, paths ...string) []string {
	t.Helper()

	relatives := make([]string, 0, len(paths))
	for _, path := range paths {
		relative, err := mirror.Relative(path)
		if err != nil {
			t.Fatal(err)
		}

		relatives = append(relatives, relative)
	}

	slices.Sort(relatives)

	return relatives
}

func checkCommitted(t *testing.T, repository *gitrepo.Repository, hash string, want ...string) {
	t.Helper()

	//nolint:gosec // Git reads only this test's private repository.
	out, err := exec.CommandContext(t.Context(), "git", "-C", repository.Path, "ls-tree", "-r", "-z", "--name-only",
		hash).Output()
	must(t, err)

	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if !slices.Equal(got, want) {
		t.Fatalf("committed %q, want %q", got, want)
	}
}

// git add rejects each of these names on Linux too, and a single one makes it stage nothing at all.
func TestGitRefusedNamesDoNotBlockReconciliation(t *testing.T) {
	t.Parallel()

	source := t.TempDir()
	repository, syncer := gitMirror(t, source)
	kept := []string{filepath.Join(source, "kept"), filepath.Join(source, "sub", "kept")}
	shortName := filepath.Join(source, "git~1")

	writeFiles(t, append([]string{
		shortName, filepath.Join(source, ".git."), filepath.Join(source, ".git ", "file"),
		filepath.Join(source, ".git::$INDEX_ALLOCATION"), filepath.Join(source, "sub", "GiT~1", "file"),
	}, kept...)...)
	must(t, os.Symlink("kept", filepath.Join(source, "sub", ".gitmodules")))
	// A copy made before the name was recognized must not keep failing every reconciliation.
	writeFiles(t, filepath.Join(repository.Path, mirrorRelative(t, shortName)[0]))

	must(t, syncer.ReconcileContext(t.Context()))

	hash, err := repository.Commit(t.Context(), "snapshot")
	if err != nil || hash == "" {
		t.Fatalf("refused names blocked the commit: %q %v", hash, err)
	}

	checkCommitted(t, repository, hash, mirrorRelative(t, kept...)...)

	if mirrored(t, repository.Path, shortName) {
		t.Fatal("reconciliation kept the earlier copy of a refused name")
	}
}

func TestGitRefusedNamesDoNotBlockEvents(t *testing.T) {
	t.Parallel()

	source := t.TempDir()
	repository, syncer := gitMirror(t, source)
	kept := filepath.Join(source, "kept")
	shortName := filepath.Join(source, "git~1")
	link := filepath.Join(source, ".gitmodules")

	writeFiles(t, kept, shortName)
	must(t, os.Symlink("kept", link))

	// Earlier copies of the refused names are already in the worktree.
	writeFiles(t, filepath.Join(repository.Path, mirrorRelative(t, shortName)[0]))
	must(t, os.Symlink("kept", filepath.Join(repository.Path, mirrorRelative(t, link)[0])))

	paths := []string{kept, shortName, link}
	must(t, syncer.ApplyPaths(t.Context(), paths))

	hash, err := repository.CommitPaths(t.Context(), "event", mirrorRelative(t, paths...))
	if err != nil || hash == "" {
		t.Fatalf("refused names blocked the commit: %q %v", hash, err)
	}

	checkCommitted(t, repository, hash, mirrorRelative(t, kept)...)

	if mirrored(t, repository.Path, shortName) || mirrored(t, repository.Path, link) {
		t.Fatal("an event kept the earlier copy of a refused name")
	}
}
