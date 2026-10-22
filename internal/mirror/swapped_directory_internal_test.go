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
