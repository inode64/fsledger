package git

import (
	"os"
	"path/filepath"
	"testing"
)

// A path below a link is not in the worktree, whatever the link points to: staged as present, Git would
// read it through the link.
//
//nolint:gocognit // Ordered filesystem fixtures cover both internal and external symlink targets.
func TestStagePathsNeverFollowSymlinkParents(t *testing.T) {
	t.Parallel()

	for _, target := range []string{"outside", "inside"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			repository := Repository{Path: t.TempDir(), Host: "test", publishable: "", Timeout: 0, exclusive: false}

			destination := t.TempDir()
			if target == "inside" {
				destination = filepath.Join(repository.Path, "real")
			}

			err := os.MkdirAll(destination, 0o700)
			if err != nil {
				t.Fatal(err)
			}

			err = os.WriteFile(filepath.Join(destination, "file"), []byte("private"), 0o600)
			if err != nil {
				t.Fatal(err)
			}

			err = os.Symlink(destination, filepath.Join(repository.Path, "parent"))
			if err != nil {
				t.Fatal(err)
			}

			present, missing, err := repository.stagePaths([]string{"parent/file"})
			if err != nil || present != "" || missing != "parent/file\x00" {
				t.Fatalf("staging followed a symlink parent: present=%q missing=%q error=%v", present, missing, err)
			}
		})
	}
}

func TestStagePathsPreservesLeafLinksAndMissingDescendants(t *testing.T) {
	t.Parallel()
	repository := Repository{Path: t.TempDir(), Host: "test", publishable: "", Timeout: 0, exclusive: false}

	err := os.WriteFile(filepath.Join(repository.Path, "replacement"), []byte("file"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Symlink("missing", filepath.Join(repository.Path, "link"))
	if err != nil {
		t.Fatal(err)
	}

	present, missing, err := repository.stagePaths([]string{"link", "replacement/old/file", "absent/child"})
	if err != nil || present != "link\x00" || missing != "replacement/old/file\x00absent/child\x00" {
		t.Fatalf("wrong staging: present=%q missing=%q error=%v", present, missing, err)
	}
}
