package restore

import (
	"archive/tar"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/resource"
)

func TestSyncDirectoryAcceptsPinnedParent(t *testing.T) {
	t.Parallel()

	parent, err := unix.Open(t.TempDir(), directoryFlags, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.FD(parent)

	// Restores pin parents through O_PATH, which fsync itself refuses.
	if !errors.Is(unix.Fsync(parent), unix.EBADF) {
		t.Fatal("fixture parent is not an O_PATH descriptor")
	}

	err = syncDirectory(parent)
	if err != nil {
		t.Fatal(err)
	}
}

func TestWriteRegularSyncsParentAfterRename(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root reopens directories regardless of their mode")
	}

	const name = "restored"

	root, parent, _ := replacementFixture(t, 0o600)

	// Write and search access let the rename through; the missing read access refuses the sync.
	err := os.Chmod(root.Name(), 0o300) //nolint:gosec // Search access is part of the mode under test.
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		//nolint:gosec // Restores the private temporary directory so the test framework can remove it.
		restoreErr := os.Chmod(root.Name(), 0o700)
		if restoreErr != nil {
			t.Error(restoreErr)
		}
	})

	//nolint:exhaustruct_v5 // Only the executable bits are consulted when writing file content.
	err = writeRegular(strings.NewReader("content"), &tar.Header{Mode: 0o644}, parent, name, nil)
	if !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), "sync") {
		t.Fatal("parent directory was not synced after the rename:", err)
	}

	content, err := root.ReadFile(name)
	if err != nil || string(content) != "content" {
		t.Fatal(string(content), err)
	}

	// Listing the leftovers needs the read access the sync was denied.
	err = os.Chmod(root.Name(), 0o700) //nolint:gosec // Restores the private temporary directory.
	if err != nil {
		t.Fatal(err)
	}

	leftovers := temporaries(t, root)
	if len(leftovers) != 0 {
		t.Fatal("temporary copy left behind:", leftovers)
	}
}
