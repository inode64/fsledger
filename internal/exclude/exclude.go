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
	for _, pattern := range patterns {
		if pattern == "" || !doublestar.ValidatePattern(pattern) {
			return nil, fault.New(fmt.Sprintf("invalid exclusion %q", pattern))
		}
	}

	return &Matcher{rules: compile(patterns), protected: nil}, nil
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
