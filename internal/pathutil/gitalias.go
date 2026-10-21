package pathutil

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// NTFS 8.3 short names keep six characters, then a tilde and a counter.
const (
	shortNameLength = 8
	shortNamePrefix = 6
)

// HasGitAlias reports components that Git treats as .git on some filesystem: .git itself and its
// NTFS and HFS+ aliases such as git~1, ".git." or ".git::$INDEX_ALLOCATION". Git rejects them from
// its index or from transferred trees even on Linux, and one rejected entry fails a whole git add.
func HasGitAlias(path string) bool {
	for part := range strings.SplitSeq(path, "/") {
		if hfsAlias(part, ".git") || ntfsAlias(part, ntfsGitAlias) {
			return true
		}
	}

	return false
}

// GitModulesAlias reports names that Git treats as .gitmodules; Git never stores a symlink under one.
func GitModulesAlias(name string) bool {
	return hfsAlias(name, ".gitmodules") || ntfsAlias(name, ntfsGitModulesAlias)
}

// ntfsAlias checks the name and every suffix after a backslash, which NTFS reads as a separator.
func ntfsAlias(name string, alias func(string) bool) bool {
	for {
		if alias(name) {
			return true
		}

		_, after, found := strings.Cut(name, `\`)
		if !found {
			return false
		}

		name = after
	}
}

// ntfsGitAlias mirrors Git's is_ntfs_dotgit: NTFS drops trailing dots and spaces, and a colon
// selects an alternate data stream of the same entry.
func ntfsGitAlias(name string) bool {
	if end := strings.IndexAny(name, `\:`); end >= 0 {
		name = name[:end]
	}

	return onlyDotsAndSpaces(name, ".git") || onlyDotsAndSpaces(name, "git~1")
}

// ntfsGitModulesAlias mirrors Git's is_ntfs_dotgitmodules, including both forms of short name.
func ntfsGitModulesAlias(name string) bool {
	const (
		long  = ".gitmodules"
		short = "gitmod~"
	)

	switch {
	case foldedPrefix(name, long):
		return streamOrDotsAndSpaces(name[len(long):])
	case foldedPrefix(name, short) && len(name) > len(short) && name[len(short)] >= '1' && name[len(short)] <= '4':
		return streamOrDotsAndSpaces(name[len(short)+1:])
	default:
		return fallbackShortName(name, "gi7eba") && streamOrDotsAndSpaces(name[shortNameLength:])
	}
}

// fallbackShortName matches the hashed 8.3 form: a prefix of at most six characters, a tilde, a
// nonzero digit, then digits up to eight characters.
func fallbackShortName(name, prefix string) bool {
	if len(name) < shortNameLength {
		return false
	}

	tilde := strings.IndexByte(name[:shortNameLength], '~')
	if tilde < 0 || tilde > shortNamePrefix || !foldedPrefix(name, prefix[:tilde]) {
		return false
	}

	counter := name[tilde+1 : shortNameLength]

	return counter != "" && counter[0] != '0' &&
		!strings.ContainsFunc(counter, func(char rune) bool { return char < '0' || char > '9' })
}

func onlyDotsAndSpaces(name, prefix string) bool {
	return foldedPrefix(name, prefix) && strings.Trim(name[len(prefix):], ". ") == ""
}

func streamOrDotsAndSpaces(rest string) bool {
	rest, _, _ = strings.Cut(rest, ":")

	return strings.Trim(rest, ". ") == ""
}

// foldedPrefix compares like strncasecmp: only ASCII letters fold, unlike strings.EqualFold.
func foldedPrefix(name, prefix string) bool {
	if len(name) < len(prefix) {
		return false
	}

	for index := range len(prefix) {
		if lowerASCII(rune(name[index])) != rune(prefix[index]) {
			return false
		}
	}

	return true
}

// hfsAlias mirrors Git's is_hfs_dot_generic: HFS+ folds case and ignores some zero-width code
// points, and Git ends the name at malformed UTF-8.
func hfsAlias(name, needle string) bool {
	for _, want := range needle {
		var char rune

		char, name = nextHFSRune(name)
		if char > unicode.MaxASCII || lowerASCII(char) != want {
			return false
		}
	}

	char, _ := nextHFSRune(name)

	return char == 0
}

// nextHFSRune returns the next code point HFS+ compares and the rest, or 0 at the end or at malformed UTF-8.
func nextHFSRune(name string) (rune, string) {
	for name != "" {
		char, size := utf8.DecodeRuneInString(name)
		if char == utf8.RuneError && size <= 1 {
			return 0, ""
		}

		name = name[size:]
		if !hfsIgnored(char) {
			return char, name
		}
	}

	return 0, ""
}

// hfsIgnored lists the joiners, directional marks and byte order mark that HFS+ drops.
func hfsIgnored(char rune) bool {
	const (
		joinersFirst, joinersLast       = '\u200c', '\u200f'
		embeddingsFirst, embeddingsLast = '\u202a', '\u202e'
		shapingFirst, shapingLast       = '\u206a', '\u206f'
		zeroWidthNoBreakSpace           = '\ufeff'
	)

	return joinersFirst <= char && char <= joinersLast || embeddingsFirst <= char && char <= embeddingsLast ||
		shapingFirst <= char && char <= shapingLast || char == zeroWidthNoBreakSpace
}

func lowerASCII(char rune) rune {
	if 'A' <= char && char <= 'Z' {
		return char + 'a' - 'A'
	}

	return char
}
