package integrity

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRootGuardAcceptsRootKindReplacementOnPinnedFilesystem(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "source")

	err := os.Mkdir(root, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	guard := RootGuard{}

	err = guard.Check([]string{root})
	if err != nil {
		t.Fatal(err)
	}

	err = os.Remove(root)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(root, []byte("file"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = guard.Check([]string{root})
	if err != nil {
		t.Fatal("directory-to-file replacement blocked the root", err)
	}

	err = os.Remove(root)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Symlink("target", root)
	if err != nil {
		t.Fatal(err)
	}

	err = guard.Check([]string{root})
	if err != nil {
		t.Fatal("file-to-symlink replacement blocked the root", err)
	}

	identity := guard.roots[root]
	identity.mount++
	identity.record.Point = filepath.Join(root, "lost")
	guard.roots[root] = identity

	err = guard.Check([]string{root})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatal("filesystem replacement was accepted", err)
	}
}

func TestRootGuardUnavailableSeparatesDeletionFromLostFilesystem(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root := filepath.Join(base, "source")

	err := os.Mkdir(root, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	guard := RootGuard{}
	if guard.Unavailable(root) {
		t.Fatal("unknown root reported unavailable")
	}

	err = guard.Check([]string{root})
	if err != nil {
		t.Fatal(err)
	}

	if guard.Unavailable(root) {
		t.Fatal("present root reported unavailable")
	}

	err = os.Remove(root)
	if err != nil {
		t.Fatal(err)
	}

	if guard.Unavailable(root) {
		t.Fatal("deletion on the pinned filesystem reported as lost filesystem")
	}
	// Simulate the ancestor living on another mount; this is not a privileged unmount test.
	identity := guard.roots[root]
	identity.mount++
	guard.roots[root] = identity

	if !guard.Unavailable(root) {
		t.Fatal("missing root below a different filesystem treated as deletion")
	}
}

func TestRootGuardForgetAllowsRootToChangeKind(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "source")

	err := os.WriteFile(path, []byte("data"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	guard := RootGuard{}

	err = guard.Check([]string{path})
	if err != nil {
		t.Fatal(err)
	}

	err = os.Remove(path)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Mkdir(path, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	guard.Forget([]string{path})

	err = guard.Check([]string{path})
	if err != nil {
		t.Fatal(err)
	}

	if !guard.roots[path].directory {
		t.Fatal("forgotten file root kept its previous kind")
	}
}

func TestRootGuardUnavailableForFileDirectlyBelowLostFilesystem(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "grub.cfg")

	err := os.WriteFile(path, []byte("data"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	guard := RootGuard{}

	err = guard.Check([]string{path})
	if err != nil {
		t.Fatal(err)
	}

	err = os.Remove(path)
	if err != nil {
		t.Fatal(err)
	}

	if guard.Unavailable(path) {
		t.Fatal("deleted file on the pinned filesystem reported as lost filesystem")
	}
	// A file root pins its parent. After an unmount the parent still exists, as the empty
	// mountpoint of another filesystem; simulate that identity change.
	identity := guard.roots[path]
	identity.mount++
	guard.roots[path] = identity

	if !guard.Unavailable(path) {
		t.Fatal("file below an unmounted mountpoint treated as deletion")
	}
}

func TestRootGuardLearnsPreviouslyUnavailableMountID(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	guard := RootGuard{}

	err := guard.Check([]string{root})
	if err != nil {
		t.Fatal(err)
	}

	identity := guard.roots[root]
	mount := identity.mount
	identity.mount = 0

	guard.roots[root] = identity

	err = guard.Check([]string{root})
	if err != nil {
		t.Fatal("a newly available mount ID poisoned the root", err)
	}

	if guard.roots[root].mount != mount {
		t.Fatal("mount ID was not learned")
	}
}

// A recorded mount point inside a root that the table no longer lists keeps its tree; one still mounted, or
// outside every root, does not.
func TestRetainedMountsAreTheUnmountedOnesWithinRoots(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	gone := filepath.Join(root, "gone")

	retained := RetainedMounts([]string{gone, "/proc", "/elsewhere"}, []string{root, "/"})
	if len(retained) != 2 || retained[0] != gone || retained[1] != "/elsewhere" {
		t.Fatal("wrong retained mount points", retained)
	}

	if retained = RetainedMounts([]string{gone}, []string{filepath.Join(root, "other")}); len(retained) != 0 {
		t.Fatal("a mount point outside the roots was retained", retained)
	}
}

// After a restart nothing is pinned. A recorded mount point that the table no longer lists, here a plain
// directory, means the source found there is its empty mount point, and a missing source below it was
// lost with it rather than deleted.
func TestRecordedMountPointNotMountedMakesTheSourceUnavailable(t *testing.T) {
	t.Parallel()

	point := t.TempDir()
	root := filepath.Join(point, "source")

	err := os.Mkdir(root, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	guard := RootGuard{}
	guard.Expect([]string{point})

	err = guard.Check([]string{root})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatal("a source on an unmounted recorded mount point was pinned", err)
	}

	missing := filepath.Join(point, "missing")
	if !guard.Unavailable(missing) || (&RootGuard{}).Unavailable(missing) {
		t.Fatal("a missing source below an unmounted recorded mount point was taken for a deletion")
	}
}
