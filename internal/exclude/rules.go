package exclude

import (
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// metacharacters are the doublestar syntax; patterns without them are literal.
const metacharacters = `*?[]{}\`

// rules evaluates every pattern against one candidate. Match visits each ancestor,
// so a simple "P/**" contributes only its directory form P: whatever "P/**"
// matches has an ancestor or itself matched by P. Simple shapes are decided with
// string operations; doublestar remains the reference for every other pattern.
type rules struct {
	// names holds "**/name": the last component.
	names map[string]struct{}
	// suffixes holds "/a/b" for "**/a/b": the relative path or a component suffix of it.
	suffixes []string
	// globs holds "**/x*y" split at '*': the last component.
	globs [][]string
	// absolute holds "/a/b": the candidate itself.
	absolute map[string]struct{}
	// children holds "/a/x*y" by parent "/a" (the empty string for "/"): the last component.
	children map[string][][]string
	fallback []fallback
}

type fallback struct {
	pattern, directory, prefix string
	trailing, absolute         bool
}

func compile(patterns []string) *rules {
	result := &rules{
		names: make(map[string]struct{}), suffixes: nil, globs: nil,
		absolute: make(map[string]struct{}), children: make(map[string][][]string), fallback: nil,
	}

	for _, pattern := range patterns {
		directory, trailing := strings.CutSuffix(pattern, "/**")
		if !result.add(directory) {
			result.fallback = append(result.fallback, fallback{
				pattern: pattern, directory: directory, trailing: trailing,
				absolute: strings.HasPrefix(pattern, "/"),
				// The directory form is a prefix of the pattern, so its literal prefix bounds both. Trailing
				// slashes are dropped: "/**" may match nothing, the slash before it included.
				prefix: strings.TrimRight(directory[:strings.IndexAny(directory+"*", metacharacters)], "/"),
			})
		}
	}

	return result
}

// add records a simple pattern and reports whether it had one of the simple shapes.
func (r *rules) add(pattern string) bool {
	if rest, ok := strings.CutPrefix(pattern, "**/"); ok && rest != "" {
		switch {
		case !strings.ContainsAny(rest, metacharacters) && !strings.Contains(rest, "/"):
			r.names[rest] = struct{}{}
		case !strings.ContainsAny(rest, metacharacters):
			r.suffixes = append(r.suffixes, "/"+rest)
		default:
			parts, simple := componentGlob(rest)
			if !simple {
				return false
			}

			r.globs = append(r.globs, parts)
		}

		return true
	}

	if !strings.HasPrefix(pattern, "/") {
		return false
	}

	if !strings.ContainsAny(pattern, metacharacters) {
		r.absolute[pattern] = struct{}{}

		return true
	}

	slash := strings.LastIndexByte(pattern, '/')
	parent, name := pattern[:slash], pattern[slash+1:]

	parts, simple := componentGlob(name)
	if strings.ContainsAny(parent, metacharacters) || !simple {
		return false
	}

	r.children[parent] = append(r.children[parent], parts)

	return true
}

// componentGlob splits one path component whose only metacharacter is a single-star
// wildcard. At least one literal part keeps it from matching an empty component.
func componentGlob(name string) ([]string, bool) {
	if strings.Contains(name, "/") || strings.Contains(name, "**") ||
		strings.ContainsAny(name, strings.Trim(metacharacters, "*")) || strings.Trim(name, "*") == "" {
		return nil, false
	}

	return strings.Split(name, "*"), true
}

func (r *rules) matches(candidate string) bool {
	relative := strings.TrimPrefix(candidate, "/")
	slash := strings.LastIndexByte(candidate, '/')
	name := candidate[slash+1:]

	if r.matchesRelative(relative, name) ||
		strings.HasPrefix(candidate, "/") && r.matchesAbsolute(candidate, candidate[:slash], name) {
		return true
	}

	for _, rule := range r.fallback {
		if rule.matches(candidate, relative) {
			return true
		}
	}

	return false
}

func (r *rules) matchesRelative(relative, name string) bool {
	if _, ok := r.names[name]; ok {
		return true
	}

	for _, suffix := range r.suffixes {
		if relative == suffix[1:] || strings.HasSuffix(relative, suffix) {
			return true
		}
	}

	for _, parts := range r.globs {
		if globMatch(parts, name) {
			return true
		}
	}

	return false
}

func (r *rules) matchesAbsolute(candidate, parent, name string) bool {
	if _, ok := r.absolute[candidate]; ok {
		return true
	}

	for _, parts := range r.children[parent] {
		if globMatch(parts, name) {
			return true
		}
	}

	return false
}

func (rule fallback) matches(candidate, relative string) bool {
	target := relative
	if rule.absolute {
		target = candidate
	}

	// Literal characters before the first metacharacter must match exactly.
	if !strings.HasPrefix(target, rule.prefix) {
		return false
	}

	// compile kept only validated patterns.
	if doublestar.MatchUnvalidated(rule.pattern, target) {
		return true
	}

	// A trailing /** also excludes the directory itself. doublestar does not match it on its own
	// when the last component ends in a wildcard that consumes nothing, as in "deps*/**" and "deps".
	return rule.trailing && doublestar.MatchUnvalidated(rule.directory, target)
}

// globMatch matches a component against parts split at '*'. Taking each middle
// part at its leftmost position is optimal when '*' is the only wildcard.
func globMatch(parts []string, name string) bool {
	last := len(parts) - 1

	rest, ok := strings.CutPrefix(name, parts[0])
	if !ok {
		return false
	}

	for _, part := range parts[1:last] {
		index := strings.Index(rest, part)
		if index < 0 {
			return false
		}

		rest = rest[index+len(part):]
	}

	return strings.HasSuffix(rest, parts[last])
}
