package mirror

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/resource"
)

const swappedName = "swapped"

// swappedDirectoryMirror mirrors a source whose directory has a child and sorts after a regular file.
func swappedDirectoryMirror(t *testing.T) (*Sync, string, string) {
	t.Helper()

	root, repository := t.TempDir(), t.TempDir()
	writeUnstableFixture(t, filepath.Join(root, "first"), "first")
	writeUnstableFixture(t, filepath.Join(root, swappedName, "child"), "child")

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	syncer, err := Open(repository, []string{root}, matcher, "sha256")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.Close(syncer) })

	mirrored := filepath.Join(repository, strings.TrimPrefix(root, "/"), swappedName)

	return syncer, filepath.Join(root, swappedName), mirrored
}

func requireMirroredLink(t *testing.T, mirrored, target string) {
	t.Helper()

	info, err := os.Lstat(mirrored)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("mirror did not keep the replacing link: %v %v", info, err)
	}

	link, err := os.Readlink(mirrored)
	if err != nil || link != target {
		t.Fatalf("mirrored link points to %q: %v", link, err)
	}
}

// A directory replaced by a link after its parent was listed is mirrored as that link, never walked through it.
func TestReconcileNeverWalksDirectorySwappedForSymlink(t *testing.T) {
	t.Parallel()

	outside := t.TempDir()
	writeUnstableFixture(t, filepath.Join(outside, "deep", "er", "secret"), "secret")

	// Opening the loop through the link fails; the tree would leave its directories in the mirror.
	targets := map[string]string{"loop": swappedName, "tree": outside}
	for name, target := range targets {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			syncer, swapped, mirrored := swappedDirectoryMirror(t)

			// The earlier sibling is copied after the root was listed and before swapped is opened.
			replaced := false
			syncer.copyContent = func(ctx context.Context, output io.Writer, input io.Reader) error {
				if !replaced {
					replaced = true

					err := errors.Join(os.RemoveAll(swapped), os.Symlink(target, swapped))
					if err != nil {
						return err
					}
				}

				return copyContent(ctx, output, input)
			}

			err := syncer.ReconcileContext(t.Context())
			if err != nil || !replaced {
				t.Fatalf("reconcile after replacing %t: %v", replaced, err)
			}

			requireMirroredLink(t, mirrored, target)
		})
	}
}

// A copy whose source parent became a link after it was inspected must keep the link mirrored for that parent.
func TestCopyBelowSwappedParentKeepsMirroredLink(t *testing.T) {
	t.Parallel()

	syncer, swapped, mirrored := swappedDirectoryMirror(t)

	err := syncer.ReconcileContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	err = errors.Join(os.RemoveAll(swapped), os.Symlink(t.TempDir(), swapped))
	if err != nil {
		t.Fatal(err)
	}

	target, err := os.Readlink(swapped)
	if err != nil {
		t.Fatal(err)
	}

	err = syncer.ReconcileContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	child := filepath.Join(swapped, "child")

	relative, err := Relative(child)
	if err != nil {
		t.Fatal(err)
	}

	err = syncer.copyStable(t.Context(), child, relative, copyEvent)
	if !integrity.Missing(err) {
		t.Fatalf("copy below a replaced parent: %v", err)
	}

	requireMirroredLink(t, mirrored, target)
}

// The walk keeps the directory whose entries it copies. Moved away while they are copied, that directory must
// not lend its entries, now outside the selection, to the path it left, nor hide the entries that replaced them.
func TestDirectoryMovedDuringCopiesIsAnchoredAgain(t *testing.T) {
	t.Parallel()

	for name, move := range directoryMoves() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			syncer, root, mirrored := movedDirectoryMirror(t)
			moved := moveOnFirstCopy(syncer, func() error { return move(root) })

			err := syncer.ReconcileContext(t.Context())
			if err != nil || !*moved {
				t.Fatalf("reconcile after moving %t: %v", *moved, err)
			}

			for _, entry := range []string{"b", "c"} {
				//nolint:gosec // Read only the mirror entry in this test's private repository.
				content, readErr := os.ReadFile(filepath.Join(mirrored, entry))
				if string(content) == "secret" ||
					readErr != nil && (name == replacedDirectory || !os.IsNotExist(readErr)) {
					t.Fatalf("mirrored %s from the moved directory: %q %v", entry, content, readErr)
				}
			}
		})
	}
}

// moveOnFirstCopy runs move once, while the walk copies its first changed entry.
func moveOnFirstCopy(syncer *Sync, move func() error) *bool {
	moved := new(bool)
	syncer.copyContent = func(ctx context.Context, output io.Writer, input io.Reader) error {
		if !*moved {
			*moved = true

			err := move()
			if err != nil {
				return err
			}
		}

		return copyContent(ctx, output, input)
	}

	return moved
}

const replacedDirectory = "replaced and deleted"

// directoryMoves moves root/p/d out of the selection: its b holds a secret where it lands, or a new d replaces it.
func directoryMoves() map[string]func(root string) error {
	return map[string]func(root string) error{
		"into an excluded directory": func(root string) error {
			return errors.Join(
				os.Rename(filepath.Join(root, "p", "d"), filepath.Join(root, "secret", "d")),
				os.WriteFile(filepath.Join(root, "secret", "d", "b"), []byte("secret"), 0o600),
				os.MkdirAll(filepath.Join(root, "p", "d"), 0o700),
			)
		},
		"behind a link to an excluded directory": func(root string) error {
			return errors.Join(
				os.Rename(filepath.Join(root, "p"), filepath.Join(root, "secret", "p")),
				os.Symlink(filepath.Join(root, "secret", "p"), filepath.Join(root, "p")),
				os.WriteFile(filepath.Join(root, "secret", "p", "d", "b"), []byte("secret"), 0o600),
			)
		},
		replacedDirectory: func(root string) error {
			directory := filepath.Join(root, "p", "d")

			return errors.Join(
				os.CopyFS(directory+".new", os.DirFS(directory)),
				os.Rename(directory, directory+".old"),
				os.Rename(directory+".new", directory),
				os.RemoveAll(directory+".old"),
			)
		},
	}
}

// movedDirectoryMirror mirrors root/p/d with three entries, then changes the first so that the next
// reconciliation copies it before the others; root/secret is excluded.
func movedDirectoryMirror(t *testing.T) (*Sync, string, string) {
	t.Helper()

	root, repository := t.TempDir(), t.TempDir()
	for _, entry := range []string{"a", "b", "c"} {
		writeUnstableFixture(t, filepath.Join(root, "p", "d", entry), entry)
	}

	err := os.Mkdir(filepath.Join(root, "secret"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	matcher, err := exclude.New(filepath.Join(root, "secret"))
	if err != nil {
		t.Fatal(err)
	}

	syncer, err := Open(repository, []string{root}, matcher, "sha256")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.Close(syncer) })

	err = syncer.ReconcileContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	writeUnstableFixture(t, filepath.Join(root, "p", "d", "a"), "changed")

	return syncer, root, filepath.Join(repository, strings.TrimPrefix(root, "/"), "p", "d")
}
