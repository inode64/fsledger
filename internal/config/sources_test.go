package config_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/config"
)

const (
	systemRepository = "system"
	firstRepository  = "first"
	secondRepository = "second"
	wordpressPattern = "/var/www/*/htdocs/wp-config.php"
)

// sourceTree returns a symlink-free temporary directory, as resolved roots are compared textually.
func sourceTree(t *testing.T, names ...string) string {
	t.Helper()

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range names {
		path := filepath.Join(base, name)
		if strings.HasSuffix(name, "/") {
			err = os.MkdirAll(path, 0o700)
		} else {
			err = os.MkdirAll(filepath.Dir(path), 0o700)
			if err == nil {
				err = os.WriteFile(path, []byte(name), 0o600)
			}
		}

		if err != nil {
			t.Fatal(err)
		}
	}

	return base
}

func loadRepositories(t *testing.T, repositories map[string][]string, settings ...string) *config.Config {
	t.Helper()

	base := t.TempDir()
	files := make([]string, 0, len(repositories))

	for name, paths := range repositories {
		files = append(files, name+".yaml")
		writeConfiguration(t, base, name+".yaml", "paths: ['"+strings.Join(paths, "', '")+"']")
	}

	slices.Sort(files)

	cfg, err := config.Load(
		writeConfiguration(
			t,
			base,
			mainFile,
			"paths: {repositories: ["+strings.Join(files, ", ")+"]}\n"+strings.Join(settings, "\n"),
		),
	)
	if err != nil {
		t.Fatal(err)
	}

	return cfg
}

func TestResolveSourcesSkipsMissingAndExpandsPatterns(t *testing.T) {
	t.Parallel()

	tree := sourceTree(t, "etc/", "cron/root.orig", "cron/systab.orig", "cron/root", "site/a/htdocs/wp-config.php")
	cfg := loadRepositories(t, map[string][]string{systemRepository: {
		tree + "/etc", tree + "/absent/config.php", tree + "/cron/*.orig", tree + "/keys/backup*.keyfile",
		tree + "/site/*/htdocs/wp-config.php",
	}})

	sources := cfg.ResolveSources()[systemRepository]
	roots := []string{
		tree + "/cron/root.orig",
		tree + "/cron/systab.orig",
		tree + "/etc",
		tree + "/site/a/htdocs/wp-config.php",
	}
	missing := []string{tree + "/absent/config.php", tree + "/keys/backup*.keyfile"}

	if !slices.Equal(sources.Roots, roots) || !slices.Equal(sources.Missing, missing) || len(sources.Skipped) != 0 {
		t.Fatalf("unexpected resolution: %+v", sources)
	}
}

func TestResolveSourcesFollowsTrustedParentSymlinkOnly(t *testing.T) {
	t.Parallel()

	tree := sourceTree(t, "src/linux-6.18/.config", "data/target")

	for name, target := range map[string]string{"src/linux": "linux-6.18", "data/link": "target"} {
		err := os.Symlink(target, filepath.Join(tree, name))
		if err != nil {
			t.Fatal(err)
		}
	}

	cfg := loadRepositories(
		t,
		map[string][]string{systemRepository: {tree + "/src/linux/.config", tree + "/data/link"}},
	)

	sources := cfg.ResolveSources()[systemRepository]
	if !slices.Equal(sources.Roots, []string{tree + "/data/link", tree + "/src/linux-6.18/.config"}) {
		t.Fatalf("parent symlink not resolved or final symlink followed: %+v", sources)
	}

	sources = cfg.ResolveSourcesTrusting(func(uint32) bool { return false })[systemRepository]
	if !slices.Equal(sources.Roots, []string{tree + "/data/link"}) || len(sources.Skipped) != 1 ||
		!strings.Contains(sources.Skipped[0], "src/linux/.config") {
		t.Fatalf("untrusted parent symlink followed: %+v", sources)
	}
}

func TestResolveSourcesWalksDotDotInsideSymlinkTargets(t *testing.T) {
	t.Parallel()

	tree := sourceTree(t, "base/safe/file", "outside/nested/", "outside/safe/file")

	err := os.Symlink(filepath.Join(tree, "outside", "nested"), filepath.Join(tree, "base", "jump"))
	if err == nil {
		err = os.Symlink("jump/../safe", filepath.Join(tree, "base", "first"))
	}

	if err != nil {
		t.Fatal(err)
	}

	cfg := loadRepositories(t, map[string][]string{systemRepository: {tree + "/base/first/file"}})
	sources := cfg.ResolveSources()[systemRepository]
	want := filepath.Join(tree, "outside", "safe", "file")

	if !slices.Equal(sources.Roots, []string{want}) {
		t.Fatalf("symlink target .. was resolved lexically: %+v", sources)
	}

	checks := 0

	sources = cfg.ResolveSourcesTrusting(func(uint32) bool {
		checks++

		return checks == 1
	})[systemRepository]
	if len(sources.Roots) != 0 || len(sources.Skipped) != 1 || checks != 2 {
		t.Fatalf("intermediate symlink ownership was skipped: checks=%d sources=%+v", checks, sources)
	}
}

func TestResolveSourcesReportsIncompleteGlob(t *testing.T) {
	t.Parallel()

	tree := sourceTree(t, "selected/file")

	err := os.Symlink("loop", filepath.Join(tree, "loop"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := loadRepositories(t, map[string][]string{systemRepository: {tree + "/selected", tree + "/loop/*"}})
	sources := cfg.ResolveSources()[systemRepository]

	if !sources.Incomplete || len(sources.Skipped) != 1 || len(sources.Missing) != 0 ||
		!slices.Equal(sources.Roots, []string{tree + "/selected"}) {
		t.Fatalf("glob I/O failure was hidden: %+v", sources)
	}
}

func TestResolveSourcesCompactsNestedRoots(t *testing.T) {
	t.Parallel()

	tree := sourceTree(t, "www/auto/htdocs/wp-config.php", "www/other/htdocs/wp-config.php")
	cfg := loadRepositories(
		t,
		map[string][]string{systemRepository: {tree + "/www/auto/htdocs", tree + "/www/*/htdocs/wp-config.php"}},
	)

	sources := cfg.ResolveSources()[systemRepository]
	if !slices.Equal(sources.Roots, []string{tree + "/www/auto/htdocs", tree + "/www/other/htdocs/wp-config.php"}) {
		t.Fatalf("nested root kept: %+v", sources)
	}
}

func TestResolveSourcesSkipsUnsafeMatches(t *testing.T) {
	t.Parallel()

	tree := sourceTree(t, "shared/file", "state/system/file", "mods/")

	err := unix.Mkfifo(filepath.Join(tree, "mods", "pipe"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	for name, target := range map[string]string{"mods/other": "shared", "mods/reserved": "state/system"} {
		err = os.Symlink(filepath.Join(tree, target), filepath.Join(tree, name))
		if err != nil {
			t.Fatal(err)
		}
	}

	cfg := loadRepositories(t, map[string][]string{
		firstRepository:  {tree + "/shared"},
		secondRepository: {tree + "/mods/pipe", tree + "/mods/other/file", tree + "/mods/reserved/file"},
	}, "storage: {path: "+tree+"/state}")

	resolved := cfg.ResolveSources()
	if !slices.Equal(resolved[firstRepository].Roots, []string{tree + "/shared"}) {
		t.Fatalf("first repository lost its root: %+v", resolved[firstRepository])
	}

	second := resolved[secondRepository]
	if len(second.Roots) != 0 || len(second.Missing) != 0 || len(second.Skipped) != 3 {
		t.Fatalf("unsafe matches were not skipped: %+v", second)
	}

	// A worker resolves only what can claim its sources; the result must not depend on that shortcut.
	for name, sources := range resolved {
		single := cfg.ResolveRepository(name)
		if !slices.Equal(single.Roots, sources.Roots) || !slices.Equal(single.Skipped, sources.Skipped) ||
			!slices.Equal(single.Missing, sources.Missing) {
			t.Fatalf("%s resolved alone as %+v, together as %+v", name, single, sources)
		}
	}

	for index, reason := range []string{"another repository", "unsupported", "reserved state"} {
		if !strings.Contains(second.Skipped[index], reason) {
			t.Fatalf("skip %d lacks %q: %+v", index, reason, second.Skipped)
		}
	}
}

func TestSourcePatternOverlap(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		repositories map[string][]string
		name         string
		valid        bool
	}{
		{name: "pattern below literal in one repository", valid: true, repositories: map[string][]string{
			systemRepository: {"/var/www/auto/htdocs", wordpressPattern},
		}},
		{name: "disjoint patterns", valid: true, repositories: map[string][]string{
			firstRepository: {wordpressPattern}, secondRepository: {"/var/www/*/htdocs/wp-content"},
		}},
		{name: "pattern below literal across repositories", valid: false, repositories: map[string][]string{
			firstRepository: {"/var/www/auto/htdocs"}, secondRepository: {wordpressPattern},
		}},
		{name: "patterns across repositories", valid: false, repositories: map[string][]string{
			firstRepository: {"/var/spool/*/root.orig"}, secondRepository: {"/var/spool/fcron/*.orig"},
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			base := t.TempDir()
			files := make([]string, 0, len(testCase.repositories))

			for name, paths := range testCase.repositories {
				files = append(files, name+".yaml")
				writeConfiguration(t, base, name+".yaml", "paths: ['"+strings.Join(paths, "', '")+"']")
			}

			_, err := config.Load(
				writeConfiguration(t, base, mainFile, "paths: {repositories: ["+strings.Join(files, ", ")+"]}"),
			)
			if (err == nil) != testCase.valid {
				t.Fatalf("valid=%v, got %v", testCase.valid, err)
			}
		})
	}
}

func TestContentAndIntegrityRepositoriesMayShareSources(t *testing.T) {
	t.Parallel()

	tree := sourceTree(t, "www/site/htdocs/wp-config.php", "www/site/htdocs/index.php")
	base := t.TempDir()

	for name, text := range map[string]string{
		"backup.yaml":   "type: git\npaths: ['" + tree + "/www/*/htdocs/wp-config.php']",
		"site.yaml":     "type: db\npaths: ['" + tree + "/www/site/htdocs']",
		"mirror.yaml":   "type: git\npaths: ['" + tree + "/www/site']",
		"verifier.yaml": "type: db\npaths: ['" + tree + "/www/site/htdocs/index.php']",
	} {
		writeConfiguration(t, base, name, text)
	}

	cfg, err := config.Load(writeConfiguration(t, base, mainFile, "paths: {repositories: [backup.yaml, site.yaml]}"))
	if err != nil {
		t.Fatal("a Git copy and a DB inventory of the same files were rejected:", err)
	}

	resolved := cfg.ResolveSources()
	if !slices.Equal(resolved["backup"].Roots, []string{tree + "/www/site/htdocs/wp-config.php"}) ||
		!slices.Equal(resolved["site"].Roots, []string{tree + "/www/site/htdocs"}) ||
		len(resolved["backup"].Skipped)+len(resolved["site"].Skipped) != 0 {
		t.Fatalf("shared source lost: %+v", resolved)
	}

	for _, pair := range []string{"[backup.yaml, mirror.yaml]", "[site.yaml, verifier.yaml]"} {
		_, err = config.Load(writeConfiguration(t, base, mainFile, "paths: {repositories: "+pair+"}"))
		if err == nil {
			t.Fatal("repositories of one type share sources:", pair)
		}
	}
}

// Validation compares a path remote with source patterns only; a symlinked parent can still lead into it.
func TestResolveSourcesSkipsLocalRemoteBehindSymlink(t *testing.T) {
	t.Parallel()

	tree := sourceTree(t, "remote/repo.git/HEAD", "mods/")

	err := os.Symlink(filepath.Join(tree, "remote"), filepath.Join(tree, "mods", "link"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := loadRepositories(
		t,
		map[string][]string{systemRepository: {tree + "/mods/link/repo.git"}},
		"storage: {git: {remote: "+tree+"/remote/repo.git}}",
	)

	sources := cfg.ResolveSources()[systemRepository]
	if len(sources.Roots) != 0 || len(sources.Skipped) != 1 || !strings.Contains(sources.Skipped[0], "Git remote") {
		t.Fatalf("local remote watched through a symlinked parent: %+v", sources)
	}
}

// A path remote named through a link resolves like a source: a trusted link leads to the remote, and a link
// another user owns leads nowhere, or pointing it at / would skip every source.
func TestResolveSourcesFollowsOnlyTrustedRemoteLinks(t *testing.T) {
	t.Parallel()

	tree := sourceTree(t, "remote/repo.git/HEAD", "etc/passwd")

	err := os.Symlink(filepath.Join(tree, "remote", "repo.git"), filepath.Join(tree, "repo.git"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := loadRepositories(
		t,
		map[string][]string{systemRepository: {tree + "/remote", tree + "/etc"}},
		"storage: {git: {remote: "+tree+"/repo.git}}",
	)

	sources := cfg.ResolveSources()[systemRepository]
	if !slices.Equal(sources.Roots, []string{tree + "/etc"}) || len(sources.Skipped) != 1 {
		t.Fatalf("source containing the remote behind a trusted link was watched: %+v", sources)
	}

	sources = cfg.ResolveSourcesTrusting(func(uint32) bool { return false })[systemRepository]
	if len(sources.Roots) != 2 || len(sources.Skipped) != 0 {
		t.Fatalf("untrusted remote link redirected the sources: %+v", sources)
	}
}

// Git follows a path remote's links as root: a link another user owns must stop publication and fetching,
// not steer them, and its target escapes the check that keeps remotes out of sources.
func TestGitRemoteFollowsOnlyTrustedLinks(t *testing.T) {
	t.Parallel()

	tree := sourceTree(t, "remote/repo.git/HEAD")
	link := filepath.Join(tree, "repo.git")

	err := os.Symlink(filepath.Join(tree, "remote", "repo.git"), link)
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := config.GitRemote(link)
	if err != nil || resolved != filepath.Join(tree, "remote", "repo.git") {
		t.Fatal("trusted remote link not resolved", resolved, err)
	}

	_, err = config.GitRemoteTrusting(link, func(uint32) bool { return false })
	if err == nil || !strings.Contains(err.Error(), "untrusted symlink") {
		t.Fatal("untrusted remote link followed", err)
	}

	resolved, err = config.GitRemote("origin")
	if err != nil || resolved != "origin" {
		t.Fatal("remote name changed", resolved, err)
	}
}
