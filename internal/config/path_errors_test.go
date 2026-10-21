package config_test

import (
	"cmp"
	"os"
	"path/filepath"
	"testing"
)

// A rejected log, storage or runtime location names its own role and the component at fault,
// so a symlinked log directory is not reported as a storage or runtime problem.
func TestCheckPathsNamesRejectedLocation(t *testing.T) {
	t.Parallel()

	tree := sourceTree(t, "real/sub/", "file")

	err := os.Symlink("real", filepath.Join(tree, "link"))
	if err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(tree, "link")

	for _, testCase := range []struct {
		name, runtime, storage, log, want string
	}{
		{"accepted", "", "", "", ""},
		{"symlinked log directory", "", "", link + "/daemon.log", "log directory " + link + " is a symlink"},
		{
			"missing log directory below a symlink", "", "", link + "/logs/daemon.log",
			"log directory " + link + "/logs parent " + link + " is a symlink",
		},
		{"symlinked storage", "", link, "", "storage " + link + " is a symlink"},
		{"runtime file", tree + "/file", "", "", "runtime " + tree + "/file is not a directory"},
		{
			"runtime below a symlink", link + "/sub", "", "",
			"runtime " + link + "/sub resolves through a symlink to " + tree + "/real/sub",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			cfg := loadRepositories(
				t,
				map[string][]string{systemRepository: {"/srv/www"}},
				"runtime: "+cmp.Or(testCase.runtime, tree+"/run"),
				"storage: {path: "+cmp.Or(testCase.storage, tree+"/state")+"}",
				"logging: {file: "+cmp.Or(testCase.log, tree+"/logs/daemon.log")+"}",
			)

			got := ""

			checkErr := cfg.CheckPaths()
			if checkErr != nil {
				got = checkErr.Error()
			}

			// The security policy is unchanged: every symlinked location is still rejected.
			if got != testCase.want {
				t.Fatalf("error=%q, want %q", got, testCase.want)
			}
		})
	}
}

// A symlink loop in a trusted parent reads as the kernel's loop error with the link, not as distrust.
func TestResolveSourcesNamesSymlinkLoop(t *testing.T) {
	t.Parallel()

	tree := sourceTree(t, "selected/file")
	loop := filepath.Join(tree, "loop")

	err := os.Symlink("loop", loop)
	if err != nil {
		t.Fatal(err)
	}

	cfg := loadRepositories(t, map[string][]string{systemRepository: {loop + "/file"}})
	sources := cfg.ResolveSources()[systemRepository]

	want := loop + "/file: follow source parent " + loop + ": too many levels of symbolic links"
	if len(sources.Skipped) != 1 || sources.Skipped[0] != want {
		t.Fatalf("skipped=%q, want %q", sources.Skipped, want)
	}
}
