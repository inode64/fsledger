package pathutil_test

import (
	"testing"

	"github.com/inode64/fsledger/internal/pathutil"
)

// Expectations come from git 2.55 on Linux: git add rejects the NTFS aliases and fsck flags the HFS+ ones.
func TestHasGitAlias(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		".git", "srv/.GIT/config", "git~1", "srv/GiT~1/file", ".git.", ".git ", ".git. .", ".git::$INDEX_ALLOCATION",
		"git~1:stream", "Git~1. ", `name\.git`, `a\b\.GIT.`, `.git\name`, ".git\u200c", ".\u200cgit\ufeff",
		"\ufeff.git", ".git\xff",
	} {
		if !pathutil.HasGitAlias(path) {
			t.Errorf("missed Git alias %q", path)
		}
	}

	for _, path := range []string{
		"git~2", "git~1x", "xgit~1", ".gitx", ".git.x", ".gi", ".git\u200c.", ".\xff", "gitmod~1", ".gitmodules",
	} {
		if pathutil.HasGitAlias(path) {
			t.Errorf("reserved ordinary name %q", path)
		}
	}
}

func TestGitModulesAlias(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		".gitmodules", ".GitModules", ".gitmodules.", ".gitmodules ", ".gitmodules:stream", "gitmod~1", "GITMOD~4",
		"gi7eba~1", "gi7eba~9", "gi7eb~12", "~1234567", `name\.gitmodules`, ".gitmodules\u200c",
		".git\u200cmodules", ".gitmodules\xff",
	} {
		if !pathutil.GitModulesAlias(name) {
			t.Errorf("missed .gitmodules alias %q", name)
		}
	}

	for _, name := range []string{
		"gitmod~5", "gi7ebb~1", "gi7eba~0", "gi7eba~", ".gitmodulesx", "gitmodu~1", `.gitmodules\name`,
		".gitmodule", ".gitmodule\u017f", ".git",
	} {
		if pathutil.GitModulesAlias(name) {
			t.Errorf("reserved ordinary name %q", name)
		}
	}
}
