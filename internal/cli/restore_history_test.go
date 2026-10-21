package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRestoreReportsMirrorWithoutHistory(t *testing.T) {
	t.Parallel()

	root, invoke := restoreFixture(t)
	mirror := filepath.Join(root, "state", restoreRepoName)

	// A mirror initialized but never committed to, as an inventory leaves it, has an unborn HEAD.
	err := os.RemoveAll(filepath.Join(mirror, ".git"))
	if err != nil {
		t.Fatal(err)
	}

	gitRun(t, mirror, time.Time{}, "init", "-q")

	output, err := invoke(fixtureRepoFlag, restoreRepoName, dateFlag, "2026-03-04", motdPath,
		filepath.Join(root, "out", "motd"))
	if err == nil || !strings.Contains(err.Error(), "no recorded history yet: "+restoreRepoName) {
		t.Fatalf("%q %v", output, err)
	}

	_, err = os.Lstat(filepath.Join(root, "out"))
	if err == nil {
		t.Fatal("a mirror without history created the destination")
	}
}
