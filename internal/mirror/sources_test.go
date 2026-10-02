package mirror_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/mirror"
	"github.com/inode64/fsledger/internal/resource"
)

func mirrored(t *testing.T, repository, path string) bool {
	t.Helper()

	relative, err := mirror.Relative(path)
	if err != nil {
		t.Fatal(err)
	}

	_, err = os.Lstat(filepath.Join(repository, relative))

	return err == nil
}

func writeFiles(t *testing.T, paths ...string) {
	t.Helper()

	for _, path := range paths {
		err := os.MkdirAll(filepath.Dir(path), 0o700)
		if err == nil {
			err = os.WriteFile(path, []byte(path), 0o600)
		}

		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestSetSourcesPrunesDroppedRootAndAdoptsNewOne(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	repository := t.TempDir()
	kept := filepath.Join(base, "kept", "file")
	dropped := filepath.Join(base, "dropped", "file")
	added := filepath.Join(base, "added", "file")

	writeFiles(t, kept, dropped, added)

	allow, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	syncer, err := mirror.Open(repository, []string{filepath.Dir(kept), filepath.Dir(dropped)}, allow, "sha256")
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(syncer)

	err = syncer.ReconcileContext(t.Context())
	if err != nil || !mirrored(t, repository, dropped) || mirrored(t, repository, added) {
		t.Fatal("initial mirror is wrong", err)
	}

	// A retained root that vanished keeps the mirror untouched.
	err = os.RemoveAll(filepath.Dir(dropped))
	if err != nil {
		t.Fatal(err)
	}

	err = syncer.ReconcileContext(t.Context())
	if !errors.Is(err, integrity.ErrUnavailable) || !mirrored(t, repository, dropped) {
		t.Fatal("unavailable root was pruned", err)
	}

	syncer.SetSources([]string{filepath.Dir(added), filepath.Dir(kept)})

	err = syncer.ReconcileContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if mirrored(t, repository, dropped) || !mirrored(t, repository, added) || !mirrored(t, repository, kept) {
		t.Fatal("source change not reflected in the mirror")
	}
}
