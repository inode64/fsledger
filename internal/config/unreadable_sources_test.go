package config_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestResolveSourcesReportsUnreadableEntryIncomplete(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root reads directories regardless of their mode")
	}

	tree := sourceTree(t, "selected/file", "denied/source/file")
	denied := filepath.Join(tree, "denied")

	err := os.Chmod(denied, 0)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		//nolint:gosec // Restores the private temporary directory so the test framework can remove it.
		restoreErr := os.Chmod(denied, 0o700)
		if restoreErr != nil {
			t.Error(restoreErr)
		}
	})

	cfg := loadRepositories(t, map[string][]string{systemRepository: {tree + "/selected", denied + "/source"}})
	sources := cfg.ResolveSources()[systemRepository]

	// A denied parent hides whether the source still exists, so it must not read as deleted.
	if !sources.Incomplete || len(sources.Skipped) != 1 || len(sources.Missing) != 0 ||
		!slices.Equal(sources.Roots, []string{tree + "/selected"}) {
		t.Fatalf("unreadable literal source looked deleted: %+v", sources)
	}
}

func TestResolveSourcesReportsSymlinkLimitIncomplete(t *testing.T) {
	t.Parallel()

	tree := sourceTree(t, "selected/file")

	err := os.Symlink("loop", filepath.Join(tree, "loop"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := loadRepositories(t, map[string][]string{systemRepository: {tree + "/selected", tree + "/loop/file"}})
	sources := cfg.ResolveSources()[systemRepository]

	if !sources.Incomplete || len(sources.Skipped) != 1 ||
		!slices.Equal(sources.Roots, []string{tree + "/selected"}) {
		t.Fatalf("symlink limit in a literal source looked deleted: %+v", sources)
	}
}

func TestResolveSourcesKeepsAbsentAndRefusedEntriesComplete(t *testing.T) {
	t.Parallel()

	tree := sourceTree(t, "selected/file", "target/file")

	err := os.Symlink("target", filepath.Join(tree, "link"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := loadRepositories(
		t,
		map[string][]string{systemRepository: {tree + "/selected", tree + "/absent", tree + "/link/file"}},
	)

	// An absent entry and an untrusted parent link are definite answers, not unread ones.
	sources := cfg.ResolveSourcesTrusting(func(uint32) bool { return false })[systemRepository]
	if sources.Incomplete || len(sources.Skipped) != 1 || !slices.Equal(sources.Missing, []string{tree + "/absent"}) {
		t.Fatalf("absent or refused source marked the selection incomplete: %+v", sources)
	}
}
