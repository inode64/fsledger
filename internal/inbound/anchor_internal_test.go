package inbound

import (
	"errors"
	"path/filepath"
	"testing"
)

// The descriptor opened for writing must be the pinned root, not whatever its path names by then.
func TestAnchoredParentRejectsAnotherRoot(t *testing.T) {
	t.Parallel()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	pinned, err := pinRoot(root)
	if err != nil {
		t.Skip("mount identity unavailable:", err)
	}

	pinned.Inode++

	_, _, err = openAnchoredParent(pinned, filepath.Join(root, "file"), false)
	if !errors.Is(err, ErrConflict) {
		t.Fatal("replaced root accepted", err)
	}
}
