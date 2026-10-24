// Package pathutil shares lexical path predicates without filesystem access.
package pathutil

import (
	"path/filepath"
	"slices"
	"strings"
)

// ValidAbsolute accepts canonical absolute paths without NUL bytes.
func ValidAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, 0)
}

// ValidRelative accepts canonical local entry paths without NUL bytes or the root itself.
func ValidRelative(path string) bool {
	return filepath.IsLocal(path) && filepath.Clean(path) == path && path != "." && !strings.ContainsRune(path, 0)
}

// HasGitComponent reserves Git metadata case-insensitively at every depth.
func HasGitComponent(path string) bool {
	for part := range strings.SplitSeq(path, "/") {
		if strings.EqualFold(part, ".git") {
			return true
		}
	}

	return false
}

// Temporary prefixes begin the names inbound synchronization and restore stage beside their
// destinations, which may lie inside a watched source.
const (
	InboundTemporaryPrefix = ".fsledger-inbound-"
	RestoreTemporaryPrefix = ".fsledger-restore-"
)

// HasTemporaryComponent reserves fsledger's own temporaries at every depth, so one left by a crash
// or observed mid-operation is never recorded as a source entry. Unlike .git, case is significant.
func HasTemporaryComponent(path string) bool {
	for part := range strings.SplitSeq(path, "/") {
		if strings.HasPrefix(part, InboundTemporaryPrefix) || strings.HasPrefix(part, RestoreTemporaryPrefix) {
			return true
		}
	}

	return false
}

// Contains compares components of already canonical paths, not textual prefixes. An empty
// parent, such as an unset optional path, contains nothing.
func Contains(parent, child string) bool {
	if parent == "" || parent == child {
		return parent != ""
	}

	parent = strings.TrimSuffix(parent, "/")

	return len(child) > len(parent) && child[len(parent)] == '/' && strings.HasPrefix(child, parent)
}

// Within reports whether a canonical path belongs to any configured root.
func Within(roots []string, path string) bool {
	for _, root := range roots {
		if Contains(root, path) {
			return true
		}
	}

	return false
}

// Outside returns the paths that no root contains, in their order.
func Outside(paths, roots []string) []string {
	var outside []string

	for _, path := range paths {
		if !Within(roots, path) {
			outside = append(outside, path)
		}
	}

	return outside
}

// CompactRoots sorts canonical roots and removes duplicates and roots covered by an ancestor.
func CompactRoots(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}

	selected := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		selected[path] = struct{}{}
	}

	result := make([]string, 0, len(selected))
	for path := range selected {
		covered := false

		for parent := filepath.Dir(path); parent != path; parent = filepath.Dir(parent) {
			if _, exists := selected[parent]; exists {
				covered = true

				break
			}

			if parent == filepath.Dir(parent) {
				break
			}
		}

		if !covered {
			result = append(result, path)
		}
	}

	slices.Sort(result)

	return result
}
