package mirror_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/resource"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/mirror"
)

func TestRelative(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"relative", "/etc/../tmp", "/etc//x", "/", "/etc/.git/config"} {
		_, relativeErr := mirror.Relative(path)
		if relativeErr == nil {
			t.Errorf("accepted %q", path)
		}
	}

	got, err := mirror.Relative("/etc/a weird\nname")
	if err != nil || got != "etc/a weird\nname" {
		t.Fatalf("mapping: %q %v", got, err)
	}
}

//nolint:cyclop,funlen,gocognit,gocyclo // The full lifecycle keeps ordered filesystem/Git assertions together.
func TestSynchronization(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	repository := t.TempDir()
	outside := t.TempDir()

	matcher, err := exclude.New("**/*.secret")
	if err != nil {
		t.Fatal(err)
	}

	syncer, err := mirror.Open(repository, []string{source}, matcher, "sha256")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		closeErr := syncer.Close()
		if closeErr != nil {
			t.Error(closeErr)
		}
	})

	write := func(path, value string) {
		t.Helper()

		writeErr := os.WriteFile(path, []byte(value), 0o600)
		if writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	src := filepath.Join(source, "file")
	write(src, "one")
	write(filepath.Join(source, "hidden.secret"), "secret")

	mkdirErr := os.Mkdir(filepath.Join(source, "directory"), 0o700)
	if mkdirErr != nil {
		t.Fatal(mkdirErr)
	}

	write(filepath.Join(outside, "private"), "do not read")

	symlinkErr := os.Symlink(outside, filepath.Join(source, "link"))
	if symlinkErr != nil {
		t.Fatal(symlinkErr)
	}

	reconcileErr := syncer.ReconcileContext(t.Context())
	if reconcileErr != nil {
		t.Fatal(reconcileErr)
	}

	mirrored := filepath.Join(repository, strings.TrimPrefix(source, "/"))

	readlinkTarget, readlinkErr := os.Readlink(filepath.Join(mirrored, "link"))
	if readlinkErr != nil || readlinkTarget != outside {
		t.Fatalf("symlink not preserved: %q %v", readlinkTarget, readlinkErr)
	}

	applyErr := syncer.ApplyPaths(t.Context(), []string{filepath.Join(source, "link", "private")})
	if applyErr != nil {
		t.Fatal(applyErr)
	}

	// A name below a symlinked parent is absent: nothing is read through the link.
	readlinkTarget, readlinkErr = os.Readlink(filepath.Join(mirrored, "link"))
	if readlinkErr != nil || readlinkTarget != outside {
		t.Fatalf("followed source parent symlink: %q %v", readlinkTarget, readlinkErr)
	}

	write(src, "two")

	applyErr2 := syncer.ApplyPaths(t.Context(), []string{src})
	if applyErr2 != nil {
		t.Fatal(applyErr2)
	}

	//nolint:gosec // This test uses only its private temporary directory and the current test process.
	data, err := os.ReadFile(filepath.Join(mirrored, "file"))
	if err != nil || string(data) != "two" {
		t.Fatalf("modify failed: %s %v", data, err)
	}

	weirdName := "new\nname"

	renamed := filepath.Join(source, weirdName)

	renameErr := os.Rename(src, renamed)
	if renameErr != nil {
		t.Fatal(renameErr)
	}

	applyErr3 := syncer.ApplyPaths(t.Context(), []string{src})
	if applyErr3 != nil {
		t.Fatal(applyErr3)
	}

	applyErr4 := syncer.ApplyPaths(t.Context(), []string{renamed})
	if applyErr4 != nil {
		t.Fatal(applyErr4)
	}

	_, lstatErr := os.Lstat(filepath.Join(mirrored, "file"))
	if !os.IsNotExist(lstatErr) {
		t.Fatal("old name remains")
	}

	_, lstatErr2 := os.Lstat(filepath.Join(mirrored, "hidden.secret"))
	if !os.IsNotExist(lstatErr2) {
		t.Fatal("excluded file copied")
	}

	removeErr := os.Remove(renamed)
	if removeErr != nil {
		t.Fatal(removeErr)
	}

	reconcileErr2 := syncer.ReconcileContext(t.Context())
	if reconcileErr2 != nil {
		t.Fatal(reconcileErr2)
	}

	_, lstatErr3 := os.Lstat(filepath.Join(mirrored, weirdName))
	if !os.IsNotExist(lstatErr3) {
		t.Fatal("delete not reconciled")
	}
}

func TestExclusionRemovesCurrentTreeNotHistory(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	repository := t.TempDir()

	path := filepath.Join(source, "secret")

	writeFileErr := os.WriteFile(path, []byte("sensitive"), 0o600)
	if writeFileErr != nil {
		t.Fatal(writeFileErr)
	}

	allow, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	syncer, err := mirror.Open(repository, []string{source}, allow, "sha256")
	if err != nil {
		t.Fatal(err)
	}

	reconcileErr3 := syncer.ReconcileContext(t.Context())
	if reconcileErr3 != nil {
		t.Fatal(reconcileErr3)
	}

	closeErr := syncer.Close()
	if closeErr != nil {
		t.Fatal(closeErr)
	}

	deny, err := exclude.New(path)
	if err != nil {
		t.Fatal(err)
	}

	syncer, err = mirror.Open(repository, []string{source}, deny, "sha256")
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(syncer)

	reconcileErr4 := syncer.ReconcileContext(t.Context())
	if reconcileErr4 != nil {
		t.Fatal(reconcileErr4)
	}

	relative, err := mirror.Relative(path)
	if err != nil {
		t.Fatal(err)
	}

	_, statErr := os.Stat(filepath.Join(repository, relative))
	if !os.IsNotExist(statErr) {
		t.Fatal("excluded content retained in worktree")
	}
}
