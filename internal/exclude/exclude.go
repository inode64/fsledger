// Package exclude implements the project's recursive glob policy.
package exclude

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/inode64/fsledger/internal/pathutil"

	"github.com/inode64/fsledger/internal/fault"

	"github.com/bmatcuk/doublestar/v4"
)

// Matcher combines global and repository-specific exclusions.
type Matcher struct {
	rules     *rules
	protected []string
}

// New validates all patterns before they can be used.
func New(patterns ...string) (*Matcher, error) {
	normalized := make([]string, 0, len(patterns))

	for _, pattern := range patterns {
		clean, err := normalize(pattern)
		if err != nil {
			return nil, err
		}

		normalized = append(normalized, clean)
	}

	return &Matcher{rules: compile(normalized), protected: nil}, nil
}

// normalize drops empty components, which clean candidates never contain: "//"
// collapses and a trailing slash, gitignore's "this directory", reduces to the
// directory, which already excludes its contents because Match checks ancestors.
// "." and ".." have no such reading and are rejected.
func normalize(pattern string) (string, error) {
	invalid := fmt.Sprintf("invalid exclusion %q", pattern)
	kept := make([]string, 0, strings.Count(pattern, "/")+1)

	for _, component := range components(pattern) {
		if component == "." || component == ".." {
			return "", fault.New(invalid + `: "." and ".." components never match; write the clean path`)
		}

		if component != "" {
			kept = append(kept, component)
		}
	}

	clean := strings.Join(kept, "/")
	if strings.HasPrefix(pattern, "/") {
		clean = "/" + clean
	}

	if clean == "" || !doublestar.ValidatePattern(clean) {
		return "", fault.New(invalid)
	}

	return clean, nil
}

// components splits at separators. A backslash escapes the next byte, so an
// escaped separator stays inside its component as written.
func components(pattern string) []string {
	var parts []string

	start, escaped := 0, false

	for index := range len(pattern) {
		switch {
		case escaped:
			escaped = false
		case pattern[index] == '\\':
			escaped = true
		case pattern[index] == '/':
			parts = append(parts, pattern[start:index])
			start = index + 1
		}
	}

	return append(parts, pattern[start:])
}

// Match checks the full absolute path and each ancestor, so excluded directories
// stay excluded during both walking and incremental synchronization.
func (m *Matcher) Match(path string) bool {
	path = filepath.Clean(path)
	if m.reserved(path) {
		return true
	}

	// Equivalent to walking filepath.Dir up to "/" or ".", without allocating.
	for candidate := path; candidate != "."; {
		if m.rules.matches(candidate) {
			return true
		}

		if candidate == "/" {
			break
		}

		switch slash := strings.LastIndexByte(candidate, '/'); slash {
		case -1:
			candidate = "."
		case 0:
			candidate = "/"
		default:
			candidate = candidate[:slash]
		}
	}

	return false
}

// MatchEntry checks only path itself. Walkers use it below a root they already
// checked with Match, because they never descend into an excluded directory.
func (m *Matcher) MatchEntry(path string) bool {
	path = filepath.Clean(path)

	return m.reserved(path) || m.rules.matches(path)
}

// Protect adds literal mandatory subtrees; metacharacters in directory names are not globs.
func (m *Matcher) Protect(paths ...string) *Matcher {
	return &Matcher{rules: m.rules, protected: append(append([]string(nil), m.protected...), paths...)}
}

func (m *Matcher) reserved(path string) bool {
	for _, reserved := range m.protected {
		if reserved != "" && pathutil.Contains(reserved, path) {
			return true
		}
	}

	return pathutil.HasGitComponent(path)
}
