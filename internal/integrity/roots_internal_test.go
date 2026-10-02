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
