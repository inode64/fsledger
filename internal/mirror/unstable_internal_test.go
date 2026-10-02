package mirror

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/resource"
)

func writeUnstableFixture(t *testing.T, path, content string) {
	t.Helper()

	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func unstableMirror(t *testing.T, directory bool) (*Sync, string, string) {
	t.Helper()
	root, destination := t.TempDir(), t.TempDir()
	hot := filepath.Join(root, "hot")

	previous := hot
	if directory {
		previous = filepath.Join(hot, "child")
	}

	writeUnstableFixture(t, previous, "previous")
	writeUnstableFixture(t, filepath.Join(root, "deleted"), "deleted")

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	syncer, err := Open(destination, []string{root}, matcher, "sha256")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { resource.Close(syncer) })

	err = syncer.ReconcileContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	err = os.RemoveAll(hot)
	if err != nil {
		t.Fatal(err)
	}

	writeUnstableFixture(t, hot, "changing")
	writeUnstableFixture(t, filepath.Join(root, "stable"), "stable")

	err = os.Remove(filepath.Join(root, "deleted"))
	if err != nil {
		t.Fatal(err)
	}

	return syncer, hot, previous
}

func mutateCopies(t *testing.T, syncer *Sync, path string, mutations int) *int {
	t.Helper()

	copies := 0
	syncer.copyContent = func(ctx context.Context, output io.Writer, input io.Reader) error {
		err := copyContent(ctx, output, input)
		if err != nil {
			return err
		}

		limited, bounded := input.(*io.LimitedReader)
		if !bounded {
			t.Fatal("expected bounded source reader")
		}

		file, ok := limited.R.(*os.File)
		if !ok || file.Name() != path {
			return nil
		}

		copies++
		if copies <= mutations {
			writeUnstableFixture(t, path, string(make([]byte, copies)))
		}

		return nil
	}

	return &copies
}

func checkMirrorContent(t *testing.T, syncer *Sync, path, want string) {
	t.Helper()

	relative, err := Relative(path)
	if err != nil {
		t.Fatal(err)
	}

	data, err := syncer.root.ReadFile(relative)
	if err != nil || string(data) != want {
		t.Fatalf("mirror %s: %q, want %q: %v", path, data, want, err)
	}
}

const refreshOperation = "refresh"

//nolint:gocognit // Exercise the same real copy failure through each public entry point and prior file type.
func TestUnstableCopiesPreservePreviousVersionAndOtherChanges(t *testing.T) {
	t.Parallel()

	for _, operation := range []string{"reconcile", "events", "overlapping-events", refreshOperation} {
		for _, directory := range []bool{false, true} {
			t.Run(operation+map[bool]string{false: "-file", true: "-directory"}[directory], func(t *testing.T) {
				t.Parallel()
				syncer, hot, previous := unstableMirror(t, directory)
				copies := mutateCopies(t, syncer, hot, 3)
				root := filepath.Dir(hot)
				stable, deleted := filepath.Join(root, "stable"), filepath.Join(root, "deleted")

				var err error

				switch operation {
				case "reconcile":
					err = syncer.ReconcileContext(t.Context())
				case "events":
					err = syncer.ApplyPaths(t.Context(), []string{hot, stable, deleted})
				case "overlapping-events":
					err = syncer.ApplyPaths(t.Context(), []string{hot, filepath.Dir(hot), stable, deleted, hot})
				case refreshOperation:
					err = syncer.RefreshContext(t.Context(), hot)
				}

				paths, fatal := integrity.SplitUnstable(err)
				if fatal != nil || len(paths) != 1 || paths[0] != hot || *copies != 3 {
					t.Fatal(paths, fatal, *copies)
				}

				checkMirrorContent(t, syncer, previous, "previous")

				if operation != refreshOperation {
					checkMirrorContent(t, syncer, stable, "stable")

					relative, relativeErr := Relative(deleted)
					if relativeErr != nil {
						t.Fatal(relativeErr)
					}

					_, statErr := syncer.root.Stat(relative)
					if !errors.Is(statErr, os.ErrNotExist) {
						t.Fatal("unrelated deletion blocked", statErr)
					}
				}

				syncer.copyContent = copyContent

				err = syncer.ReconcileContext(t.Context())
				if err != nil {
					t.Fatal(err)
				}

				checkMirrorContent(t, syncer, hot, string(make([]byte, 3)))
			})
		}
	}
}

func TestUnstableCopySucceedsOnThirdAttempt(t *testing.T) {
	t.Parallel()
	syncer, hot, _ := unstableMirror(t, false)
	copies := mutateCopies(t, syncer, hot, 2)

	err := syncer.RefreshContext(t.Context(), hot)
	if err != nil || *copies != 3 {
		t.Fatal(err, *copies)
	}

	checkMirrorContent(t, syncer, hot, string(make([]byte, 2)))
}

func TestCopyRetriesWhenSourceNameIsReplaced(t *testing.T) {
	t.Parallel()
	syncer, hot, _ := unstableMirror(t, false)

	copies := 0
	syncer.copyContent = func(ctx context.Context, output io.Writer, input io.Reader) error {
		err := copyContent(ctx, output, input)
		if err != nil {
			return err
		}

		copies++
		if copies != 1 {
			return nil
		}

		replacement := hot + ".replacement"
		writeUnstableFixture(t, replacement, "replacement")

		return os.Rename(replacement, hot)
	}

	err := syncer.RefreshContext(t.Context(), hot)
	if err != nil || copies != 2 {
		t.Fatal(err, copies)
	}

	checkMirrorContent(t, syncer, hot, "replacement")
}

func TestSourceDeletedDuringCopyDoesNotFailEventGroup(t *testing.T) {
	t.Parallel()
	syncer, hot, _ := unstableMirror(t, false)
	removed := false
	syncer.copyContent = func(ctx context.Context, output io.Writer, input io.Reader) error {
		err := copyContent(ctx, output, input)
		if err != nil || removed {
			return err
		}

		removed = true

		return os.Remove(hot)
	}

	stable := filepath.Join(filepath.Dir(hot), "stable")

	err := syncer.ApplyPaths(t.Context(), []string{hot, stable})
	if err != nil {
		t.Fatal("concurrent deletion failed the event group", err)
	}

	relative, err := Relative(hot)
	if err != nil {
		t.Fatal(err)
	}

	_, err = syncer.root.Lstat(relative)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted source remains mirrored", err)
	}

	checkMirrorContent(t, syncer, stable, "stable")
}

func TestSourceRootLostDuringCopyPreservesMirror(t *testing.T) {
	t.Parallel()
	syncer, hot, previous := unstableMirror(t, false)
	syncer.copyContent = func(ctx context.Context, output io.Writer, input io.Reader) error {
		err := copyContent(ctx, output, input)
		if err != nil {
			return err
		}

		return os.RemoveAll(filepath.Dir(hot))
	}

	err := syncer.ApplyPaths(t.Context(), []string{hot})
	if !errors.Is(err, integrity.ErrUnavailable) {
		t.Fatal("lost root treated as a source deletion", err)
	}

	checkMirrorContent(t, syncer, previous, "previous")
}
