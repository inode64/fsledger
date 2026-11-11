package mirror_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

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

	syncer, err := mirror.Open(repository.Path, []string{source}, matcher, "sha256")
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

// Git tests every component of a symlink's path, so one below a directory it reads as .gitmodules is
// refused like one with that name.
func TestSymlinkBelowGitModulesDirectoryDoesNotBlockCommits(t *testing.T) {
	t.Parallel()

	for _, directory := range []string{".gitmodules", ".GitModules", ".gitmodules:stream", "gitmod~1:x"} {
		t.Run(directory, func(t *testing.T) {
			t.Parallel()

			source := t.TempDir()
			repository, syncer := gitMirror(t, source)
			kept := []string{filepath.Join(source, "kept"), filepath.Join(source, "sub", directory, "deep", "regular")}
			link := filepath.Join(source, "sub", directory, "deep", "link")
			copied := filepath.Join(repository.Path, mirrorRelative(t, link)[0])

			writeFiles(t, kept...)
			must(t, os.Symlink("regular", link))
			// A copy made before the path was recognized must not keep failing every commit.
			must(t, os.MkdirAll(filepath.Dir(copied), 0o700))
			must(t, os.Symlink("regular", copied))

			must(t, syncer.ReconcileContext(t.Context()))

			hash, err := repository.Commit(t.Context(), "snapshot")
			if err != nil || hash == "" {
				t.Fatalf("refused symlink blocked the commit: %q %v", hash, err)
			}

			checkCommitted(t, repository, hash, mirrorRelative(t, kept...)...)

			if mirrored(t, repository.Path, link) {
				t.Fatal("reconciliation kept the earlier copy of a refused symlink")
			}

			must(t, os.Symlink("regular", copied))
			must(t, os.WriteFile(kept[0], []byte("changed"), 0o600))

			paths := []string{kept[0], link}
			must(t, syncer.ApplyPaths(t.Context(), paths))

			hash, err = repository.CommitPaths(t.Context(), "event", mirrorRelative(t, paths...))
			if err != nil || hash == "" {
				t.Fatalf("refused symlink blocked the commit: %q %v", hash, err)
			}

			checkCommitted(t, repository, hash, mirrorRelative(t, kept...)...)

			if mirrored(t, repository.Path, link) {
				t.Fatal("an event kept the earlier copy of a refused symlink")
			}
		})
	}
}

// deepName returns a name below directory whose path has exactly length bytes.
func deepName(directory string, length int) string {
	rest := length - len(directory) - 1
	levels := (rest - 1) / unix.NAME_MAX
	parents := filepath.Join(slices.Repeat([]string{strings.Repeat("d", unix.NAME_MAX-1)}, levels)...)

	return filepath.Join(parents, strings.Repeat("f", rest-levels*unix.NAME_MAX))
}

// deepRoot opens directory to reach names below it one component at a time: the kernel refuses a path of
// PATH_MAX bytes or more whole.
func deepRoot(t *testing.T, directory string) *os.Root {
	t.Helper()

	root, err := os.OpenRoot(directory)
	must(t, err)
	t.Cleanup(func() { resource.Close(root) })

	return root
}

func writeDeep(t *testing.T, root *os.Root, names ...string) {
	t.Helper()

	for _, name := range names {
		must(t, root.MkdirAll(filepath.Dir(name), 0o700))
		must(t, root.WriteFile(name, []byte(name), 0o600))
	}
}

func present(t *testing.T, root *os.Root, names ...string) bool {
	t.Helper()

	return slices.ContainsFunc(names, func(name string) bool {
		_, err := root.Lstat(name)

		return err == nil
	})
}

// Git names a worktree entry by its path below the worktree, and git add dies on one the kernel refuses for
// its length. A source path one byte short of PATH_MAX is the longest that is mirrored.
func TestPathsReachingPathMaxDoNotBlockCommits(t *testing.T) {
	t.Parallel()

	source := t.TempDir()
	repository, syncer := gitMirror(t, source)
	tree, worktree := deepRoot(t, source), deepRoot(t, repository.Path)
	kept := filepath.Join(source, "kept")
	longest := deepName(source, unix.PathMax-1)
	// Git can still stat a file that reaches PATH_MAX, but not list a directory of that length.
	reaching, beyond := deepName(source, unix.PathMax), deepName(source, unix.PathMax+1)
	refused := []string{filepath.Join(source, reaching), filepath.Join(source, beyond)}
	committed := mirrorRelative(t, kept, filepath.Join(source, longest))
	copied := mirrorRelative(t, refused...)

	writeFiles(t, kept)
	writeDeep(t, tree, longest, reaching, beyond)
	// Copies made before the length was checked must not keep failing every commit.
	writeDeep(t, worktree, copied...)

	must(t, syncer.ReconcileContext(t.Context()))

	hash, err := repository.Commit(t.Context(), "snapshot")
	if err != nil || hash == "" {
		t.Fatalf("deep paths blocked the commit: %q %v", hash, err)
	}

	checkCommitted(t, repository, hash, committed...)

	if present(t, worktree, copied...) {
		t.Fatal("reconciliation kept the earlier copy of a deep path")
	}

	writeDeep(t, worktree, copied...)
	must(t, os.WriteFile(kept, []byte("changed"), 0o600))
	must(t, syncer.ApplyPaths(t.Context(), append([]string{kept}, refused...)))

	hash, err = repository.Commit(t.Context(), "refresh")
	if err != nil || hash == "" {
		t.Fatalf("deep paths blocked the commit: %q %v", hash, err)
	}

	checkCommitted(t, repository, hash, committed...)

	if present(t, worktree, copied...) {
		t.Fatal("a refresh kept the earlier copy of a deep path")
	}
}
