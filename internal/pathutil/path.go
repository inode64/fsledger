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

// Contains compares components of already canonical paths, not textual prefixes.
func Contains(parent, child string) bool {
	return parent == child || strings.HasPrefix(child, strings.TrimSuffix(parent, "/")+"/")
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
