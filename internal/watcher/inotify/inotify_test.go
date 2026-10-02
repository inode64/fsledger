package inotify_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/resource"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/watcher/inotify"
)

//nolint:cyclop,funlen,gocognit,gocyclo // The full lifecycle keeps ordered filesystem/Git assertions together.
func TestRecursiveCreateAndRename(t *testing.T) {
	t.Parallel()
	source := t.TempDir()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector, err := inotify.New(source, matcher)
	if err != nil {
		t.Skipf("inotify unsupported: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	defer resource.Close(detector)

	startErr := detector.Start(ctx)
	if startErr != nil {
		t.Skipf("inotify unavailable: %v", startErr)
	}

	directory := filepath.Join(source, "newdir")

	mkdirErr := os.Mkdir(directory, 0o700)
	if mkdirErr != nil {
		t.Fatal(mkdirErr)
	}

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()

	for {
		select {
		case raw := <-detector.Events():
			if raw.Path == directory {
				goto watched
			}
		case <-deadline.C:
			t.Fatal("new directory not observed")
		}
	}

watched:
	original := filepath.Join(directory, "a")

	writeFileErr := os.WriteFile(original, []byte("content"), 0o600)
	if writeFileErr != nil {
		t.Fatal(writeFileErr)
	}

	renamed := filepath.Join(directory, "b")

	renameErr := os.Rename(original, renamed)
	if renameErr != nil {
		t.Fatal(renameErr)
	}

	seenOld, seenNew := false, false
	for !seenOld || !seenNew {
		select {
		case raw := <-detector.Events():
			seenOld = seenOld || raw.Path == original
			seenNew = seenNew || raw.Path == renamed
		case <-deadline.C:
			t.Fatalf("rename events old=%v new=%v", seenOld, seenNew)
		}
	}
}

//nolint:cyclop,funlen,gocognit,gocyclo // Ordered events verify that the post-rename watch is actually live.
func TestDirectoryRenameReinstallsDescendantWatches(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	original := filepath.Join(source, "old")
	nested := filepath.Join(original, "nested")

	err := os.MkdirAll(nested, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector, err := inotify.New(source, matcher)
	if err != nil {
		t.Skipf("inotify unsupported: %v", err)
	}
	defer resource.Close(detector)

	err = detector.Start(t.Context())
	if err != nil {
		t.Skipf("inotify unavailable: %v", err)
	}

	renamed := filepath.Join(source, "new")

	err = os.Rename(original, renamed)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()

	created := filepath.Join(renamed, "nested", "created")

	for {
		select {
		case raw := <-detector.Events():
			if raw.Path == renamed {
				err = os.WriteFile(created, []byte("content"), 0o600)
				if err != nil {
					t.Fatal(err)
				}

				goto written
			}
		case <-deadline.C:
			t.Fatal("renamed directory was not observed")
		}
	}

written:
	for {
		select {
		case raw := <-detector.Events():
			if raw.Path == created {
				return
			}

			if raw.Path == filepath.Join(original, "nested", "created") {
				t.Fatal("descendant event retained the old directory name")
			}
		case <-deadline.C:
			t.Fatal("descendant event missing after directory rename")
		}
	}
}
