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

// The names come from git hash-object in SHA-1 and SHA-256 repositories.
func TestBlobIDMatchesGit(t *testing.T) {
	t.Parallel()

	for _, want := range []string{
		"ce013625030ba8dba906f756967f9e9ca394464a",
		"2cf8d83d9ee29543b34a87727421fdecb7e3f3a183d337639025de576db9ebb4",
	} {
		if got := blobID(want, []byte("hello\n")); got != want {
			t.Fatalf("blob named %s, want %s", got, want)
		}
	}
}
