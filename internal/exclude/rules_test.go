package exclude_test

import (
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/pathutil"
)

// realistic resembles a host-wide exclusion list: mostly suffix and name rules.
func realistic() []string {
	return []string{
		"**/*.bak", "**/*.bak*", "**/*.old*", "**/*-$", "**/*~", "**/#*#", "**/.#*", "**/old", "**/*.log",
		"**/etc/cups/certs", "**/etc/adjtime", "**/.idea/**", "**/__pycache__/**", `**/\{arch\}`,
		"**/=RELEASE-ID", "**/root/.ssh/known_hosts*", "/var/lib/dhcp/*.leases*", "/etc/ha/deps*/**",
		"/etc/*.backup.upd-*/**", "/etc/*.copia*/**", "/etc/ssh/moduli", "/var/lib/app/locales/**",
		"/srv/site/**/cache/**", "**/tmpdir*/**", "etc/relative", "etc/rel*/**",
	}
}

// edges are checked one at a time so that a pattern matching everything cannot hide the others.
func edges() []string {
	return []string{
		"**", "/**", "**/*", "/*", "/", "*", "a", "a/**", "/a", "/a/**", "**/a*", "**/*a", "**/a*b*c", "**/*a*",
		"**/a*a", "**/ab*ba", "/etc/*", "/etc/x*/**", "/a/b*/**", "**/a/b", "**/a/b/**", "**/[ab]", "**/{a,b}",
		"/e?c/**", "**/a/**/b", "*/a", "/*/a", `**/e\?c`, "**/*/a", "**/a/*",
		"**/a/", "/a/", "a/", "**/a/b/", "/a*", "/*a*/b", "**/*.[ch]", `**/\*x`, "**/a b", "/a b/**",
		"**/a/b*", "/a/**/b*", "**/.a", "**/a.b*c", "/**/a", "**/**/a", "**/a*/**", "/a*/**", "a*",
		// "/**" may match nothing, the slash before it included.
		"/a/**/", "/a/**/**", "/etc/**/", "**/a/**/", "a/**/", "/a//**", "/a/**/b/",
	}
}

func components() []string {
	return []string{
		"a", "b", "aa", "ab", "ba", "abba", "abc", "aXbYc", "c", "etc", "x", "xa", "cups", "certs", "old", "older",
		"deps", "deps.old", "dep", ".idea", "f.bak", "f.bak1", ".bak", "a.old.b", "#x#", ".#x", "x~", "x-$",
		"known_hosts", "known_hosts.old", "{arch}", "=RELEASE-ID", "var", "lib", "dhcp", "d.leases", "leases",
		"sermo.backup.upd-1", "ha", "srv", "site", "cache", "tmpdir", "tmpdir2", "root", ".ssh", "ssh",
		"moduli", "relative", "rel1", "app", "locales", "e?c", "[ab]", "f.log", "adjtime",
		"a b", "*x", "f.c", "f.h", ".a", "a.bXc", "a.bc", "b1",
	}
}

// reference is the previous Matcher algorithm: every pattern through doublestar on every ancestor.
func reference(patterns []string, path string) bool {
	path = filepath.Clean(path)
	if pathutil.HasGitComponent(path) {
		return true
	}

	for candidate := path; candidate != "."; candidate = filepath.Dir(candidate) {
		if referenceCandidate(patterns, candidate) {
			return true
		}

		if candidate == "/" {
			break
		}
	}

	return false
}

func referenceCandidate(patterns []string, candidate string) bool {
	for _, pattern := range patterns {
		target := candidate
		if !strings.HasPrefix(pattern, "/") {
			target = strings.TrimPrefix(candidate, "/")
		}

		if doublestar.MatchUnvalidated(pattern, target) {
			return true
		}

		directory, found := strings.CutSuffix(pattern, "/**")
		if found && doublestar.MatchUnvalidated(directory, target) {
			return true
		}
	}

	return false
}

func samplePaths() []string {
	pool := components()
	paths := make([]string, 0, len(pool)*(len(pool)+1)+20000+32)
	paths = append(paths, "/", ".", "etc", "etc/relative", "etc/rel1/x", "a/b", "a")

	for _, first := range pool {
		paths = append(paths, "/"+first)
		for _, second := range pool {
			paths = append(paths, "/"+first+"/"+second)
		}
	}

	random := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // Reproducible test input, not security.
	for range 20000 {
		parts := make([]string, 3+random.IntN(4))
		for index := range parts {
			parts[index] = pool[random.IntN(len(pool))]
		}

		paths = append(paths, "/"+strings.Join(parts, "/"))
	}

	return append(
		paths,
		"/srv/site/a/b/cache/f",
		"/srv/site/cache",
		"/etc/ha/deps",
		"/etc/ha/deps.old/x",
		"/var/lib/dhcp/d.leases",
		"/var/lib/app/locales/x",
		"/root/.ssh/known_hosts.old",
		"/x/etc/cups/certs/a",
		"/etc/application.backup.upd-1/a",
	)
}

func TestCompiledRulesMatchDoublestar(t *testing.T) {
	t.Parallel()

	single := edges()
	sets := make([][]string, 0, len(single)+1)

	sets = append(sets, realistic())
	for _, pattern := range single {
		sets = append(sets, []string{pattern})
	}

	paths := samplePaths()

	for _, patterns := range sets {
		matcher, err := exclude.New(patterns...)
		if err != nil {
			t.Fatal(patterns, err)
		}

		for _, path := range paths {
			want := reference(patterns, path)
			if matcher.Match(path) != want {
				t.Errorf("%q on %q: Match=%v, doublestar=%v", patterns, path, !want, want)
			}

			// Walkers call MatchEntry only below a directory that was not excluded.
			parent := filepath.Dir(filepath.Clean(path))
			if parent != path && !reference(patterns, parent) && matcher.MatchEntry(path) != want {
				t.Errorf("%q on %q: MatchEntry=%v, doublestar=%v", patterns, path, !want, want)
			}
		}
	}
}

func BenchmarkMatch(b *testing.B) {
	patterns := realistic()
	for index := range 80 {
		patterns = append(patterns, "/etc/service"+strings.Repeat("x", index%7)+"/generated*/**")
	}

	matcher, err := exclude.New(patterns...)
	if err != nil {
		b.Fatal(err)
	}

	paths := []string{"/etc/nginx/sites-enabled/default.conf", "/var/www/site/htdocs/wp-config.php", "/etc/a.bak"}

	b.Run("doublestar", func(b *testing.B) {
		for index := range b.N {
			reference(patterns, paths[index%len(paths)])
		}
	})

	b.Run("Match", func(b *testing.B) {
		for index := range b.N {
			matcher.Match(paths[index%len(paths)])
		}
	})

	b.Run("MatchEntry", func(b *testing.B) {
		for index := range b.N {
			matcher.MatchEntry(paths[index%len(paths)])
		}
	})
}
