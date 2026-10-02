package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/pathutil"
)

const (
	sourceMetacharacters = `*?[{\`
	maximumSymlinkHops   = 40
)

// Sources is the current on-disk meaning of one repository's configured paths.
type Sources struct {
	// Roots are existing, non-overlapping sources whose parents contain no symlinks.
	Roots []string `json:"roots" yaml:"roots"`
	// Missing lists configured entries that currently match nothing.
	Missing []string `json:"missing" yaml:"missing"`
	// Skipped lists matches that must not be watched, each with its reason.
	Skipped []string `json:"skipped" yaml:"skipped"`
	// Incomplete means at least one pattern could not be read. Owners must retain
	// their previous roots because an incomplete expansion cannot prove deletion.
	Incomplete bool `json:"-" yaml:"-"`
}

// SourcePattern reports whether a configured path selects sources by pattern.
func SourcePattern(path string) bool { return strings.ContainsAny(path, sourceMetacharacters) }

// ResolveSources expands patterns and drops absent entries. It never fails: a fleet-wide
// list names paths that exist on some hosts only and may appear later.
func (c *Config) ResolveSources() map[string]Sources {
	return c.resolveSources("", func(uid uint32) bool { return uid == 0 || int(uid) == os.Geteuid() })
}

// ResolveRepository resolves one repository. Only repositories of its type named before it can
// claim its sources, so nothing else is read from disk.
func (c *Config) ResolveRepository(name string) Sources {
	return c.resolveSources(name, func(uid uint32) bool { return uid == 0 || int(uid) == os.Geteuid() })[name]
}

// resolveSources resolves every repository, or with only set, that one and those able to claim from it.
func (c *Config) resolveSources(only string, trusted func(uid uint32) bool) map[string]Sources {
	resolved := make(map[string]Sources, len(c.Repositories))
	claimed := make(map[string][]string)
	reserved := c.InternalPaths()
	// Name order makes a conflict between repositories of one type resolve identically on every evaluation.
	for _, name := range c.Names() {
		kind := c.Repositories[name].Type
		if only != "" && kind != c.Repositories[only].Type {
			continue
		}

		sources := c.resolveRepository(c.Repositories[name].Paths, claimed[kind], reserved, trusted)
		claimed[kind] = append(claimed[kind], sources.Roots...)
		resolved[name] = sources

		if name == only {
			break
		}
	}

	return resolved
}

func (c *Config) resolveRepository(entries, claimed, reserved []string, trusted func(uid uint32) bool) Sources {
	var sources Sources

	for _, entry := range entries {
		matches := []string{entry}
		if SourcePattern(entry) {
			var err error

			matches, err = doublestar.FilepathGlob(entry, doublestar.WithFailOnIOErrors())
			if err != nil {
				sources.Skipped = append(sources.Skipped, entry+": "+err.Error())
				sources.Incomplete = true

				continue
			}
		}

		found := false

		for _, match := range matches {
			root, err := c.resolveSource(match, claimed, reserved, trusted)

			switch {
			case err == nil:
				sources.Roots = append(sources.Roots, root)
			case errors.Is(err, os.ErrNotExist):
				continue
			default:
				sources.Skipped = append(sources.Skipped, match+": "+err.Error())
			}

			found = true
		}

		if !found {
			sources.Missing = append(sources.Missing, entry)
		}
	}

	sources.Roots = pathutil.CompactRoots(sources.Roots)

	slices.Sort(sources.Missing)
	slices.Sort(sources.Skipped)

	return sources
}

func (c *Config) resolveSource(
	match string,
	claimed, reserved []string,
	trusted func(uid uint32) bool,
) (string, error) {
	root, err := resolveParent(match, trusted)
	if err != nil {
		return "", err
	}

	info, err := os.Lstat(root)
	if err != nil {
		return "", fault.Wrap("inspect source", err)
	}

	if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		return "", fault.New("unsupported source type")
	}

	switch {
	case !pathutil.ValidAbsolute(root) || root == "/":
		return "", fault.New("resolves to " + root)
	case pathutil.HasGitComponent(root):
		return "", fault.New("resolves to Git metadata " + root)
	case outsideState(root, reserved) != nil:
		return "", fault.New("resolves into reserved state " + root)
	case c.Logging.File != "" && Overlaps(root, c.Logging.File):
		return "", fault.New("resolves onto the log file " + root)
	case slices.ContainsFunc(claimed, func(other string) bool { return Overlaps(root, other) }):
		return "", fault.New("resolves into another repository " + root)
	}

	return root, nil
}

// resolveParent replaces symlinked parents by their targets; the final name is never followed.
// Only links owned by a trusted user count: another owner could redirect a matched directory
// to any tree and have it copied.
func resolveParent(path string, trusted func(uid uint32) bool) (string, error) {
	resolver := parentResolver{
		current: "/",
		pending: strings.Split(strings.TrimPrefix(path, "/"), "/"),
		trusted: trusted,
		hops:    0,
	}

	for len(resolver.pending) > 1 {
		err := resolver.advance()
		if err != nil {
			return "", err
		}
	}

	return filepath.Join(resolver.current, resolver.pending[0]), nil
}

type parentResolver struct {
	trusted func(uid uint32) bool
	current string
	pending []string
	hops    int
}

func (resolver *parentResolver) advance() error {
	part := resolver.pending[0]
	resolver.pending = resolver.pending[1:]

	switch part {
	case "", ".":
		return nil
	case "..":
		resolver.current = filepath.Dir(resolver.current)

		return nil
	}

	next := filepath.Join(resolver.current, part)

	info, err := os.Lstat(next)
	if err != nil {
		return fault.Wrap("inspect source parent", err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return resolver.follow(next, info)
	}

	if !info.IsDir() {
		return fault.New("source parent is not a directory " + next)
	}

	resolver.current = next

	return nil
}

func (resolver *parentResolver) follow(path string, info os.FileInfo) error {
	resolver.hops++

	status, ok := info.Sys().(*syscall.Stat_t)
	if resolver.hops > maximumSymlinkHops || !ok || !resolver.trusted(status.Uid) {
		return fault.New("untrusted symlink in source parent " + path)
	}

	target, err := os.Readlink(path)
	if err != nil {
		return fault.Wrap("read source parent link", err)
	}

	if filepath.IsAbs(target) {
		resolver.current = "/"
	}
	// Process target components in order. Lexically cleaning a/../b here would
	// skip a when it is itself a symlink, although the kernel follows it before .. .
	components := strings.Split(strings.TrimPrefix(target, "/"), "/")
	resolver.pending = append(components, resolver.pending...)

	return nil
}
