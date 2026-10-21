package restore_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inode64/fsledger/internal/restore"
)

const (
	keysName = "authorized_keys"
	keysBody = "attacker key\n"
	linkedIn = "a/x"
)

func plantDirectory(t *testing.T, path string) {
	t.Helper()

	err := os.MkdirAll(path, 0o700)
	if err != nil {
		t.Fatal(err)
	}
}

func plantSymlink(t *testing.T, target, path string) {
	t.Helper()

	err := os.Symlink(target, path)
	if err != nil {
		t.Fatal(err)
	}
}

func assertEmpty(t *testing.T, directory string) {
	t.Helper()

	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("restore wrote through a symlink", entries, err)
	}
}

func TestApplyRefusesSymlinksInDestinationPath(t *testing.T) {
	t.Parallel()

	for _, scenario := range []struct {
		name   string
		leaf   string
		source string
		dryRun bool
	}{
		{name: "destination", leaf: "", source: prefix, dryRun: false},
		{name: "created below link", leaf: "nested", source: prefix, dryRun: false},
		{name: "single file", leaf: "restored.conf", source: prefix + "/" + configName, dryRun: false},
		{name: "dry run", leaf: "", source: prefix, dryRun: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()

			base := t.TempDir()
			privileged := filepath.Join(base, "privileged")
			plantDirectory(t, privileged)
			plantSymlink(t, privileged, filepath.Join(base, "link"))

			var output bytes.Buffer

			linked := options(restore.Off, filepath.Join(base, "link", scenario.leaf), &output)
			linked.Prefix = scenario.source
			linked.DryRun = scenario.dryRun

			_, err := restore.Apply(t.Context(), buildTar(t, file(prefix+"/"+configName, configBody)), linked)
			if err == nil || output.Len() != 0 {
				t.Fatal(output.String(), err)
			}

			assertEmpty(t, privileged)
		})
	}
}

func TestApplyFailsEntriesBelowFailedDirectories(t *testing.T) {
	t.Parallel()

	for _, dryRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "dry run"}[dryRun], func(t *testing.T) {
			t.Parallel()

			destination := t.TempDir()
			plantDirectory(t, filepath.Join(destination, "a"))
			plantDirectory(t, filepath.Join(destination, "b"))
			plantSymlink(t, "../b", filepath.Join(destination, linkedIn))

			var output bytes.Buffer

			linked := options(restore.Off, destination, &output)
			linked.DryRun = dryRun

			archived := prefix + "/" + linkedIn
			archive := buildTar(t, directory(prefix+"/a"), directory(archived), file(archived+"/"+keysName, keysBody))

			summary, err := restore.Apply(t.Context(), archive, linked)

			restored := destination + "/" + linkedIn
			if err != nil || summary != (restore.Summary{Restored: 0, Skipped: 0, Failed: 2}) ||
				!strings.Contains(output.String(), "failed "+restored+": type conflict\n") ||
				!strings.Contains(output.String(), "failed "+restored+"/"+keysName+": parent directory failed\n") {
				t.Fatal(summary, output.String(), err)
			}

			assertEmpty(t, filepath.Join(destination, "b"))
		})
	}
}

func TestApplyDoesNotFollowSymlinkedParents(t *testing.T) {
	t.Parallel()

	destination := t.TempDir()
	plantDirectory(t, filepath.Join(destination, "a"))
	plantDirectory(t, filepath.Join(destination, "b"))
	plantSymlink(t, "../b", filepath.Join(destination, linkedIn))

	// Without its directory entry nothing marks a/x as failed; the descent itself must refuse the link.
	result := apply(t, restore.Always, destination, file(prefix+"/"+linkedIn+"/"+keysName, keysBody))
	if result.summary != (restore.Summary{Restored: 0, Skipped: 0, Failed: 1}) ||
		!strings.Contains(result.output.String(), "failed "+destination+"/"+linkedIn+"/"+keysName+": ") {
		t.Fatal(result.summary, result.output.String())
	}

	assertEmpty(t, filepath.Join(destination, "b"))
}
