package config

import (
	"net/url"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/pathutil"
)

const httpsScheme = "https"

// GitRemote returns the remote Git pushes to and fetches from. A path remote is resolved through trusted
// links only, like a source: Git follows its links as root, so one another user owns could steer every
// push and fetch anywhere, and its target would escape the check that keeps remotes out of sources.
func GitRemote(remote string) (string, error) { return gitRemote(remote, trustedOwner) }

func gitRemote(remote string, trusted func(uid uint32) bool) (string, error) {
	if !filepath.IsAbs(remote) {
		return remote, nil
	}

	resolved, err := resolveParent(filepath.Clean(remote)+"/.", trusted)

	return resolved, fault.Wrap("resolve Git remote", err)
}

func (c *Config) validatePublishing() error {
	settings := c.Storage.Git
	if !validBranch(settings.Branch) {
		return fault.New("storage.git.branch must be a valid Git branch name")
	}

	if settings.PushInterval <= 0 || settings.PushTimeout <= 0 {
		return fault.New("Git push_interval and push_timeout must be positive")
	}

	if settings.FetchInterval <= 0 || settings.FetchTimeout <= 0 {
		return fault.New("Git fetch_interval and fetch_timeout must be positive")
	}

	if settings.Bidirectional && settings.Remote == "" {
		return fault.New("bidirectional Git requires storage.git.remote")
	}

	return validateRemote(settings.Remote)
}

func validateRemote(remote string) error {
	if remote == "" || validObjectName(remote) || filepath.IsAbs(remote) {
		return nil
	}

	endpoint, err := url.Parse(remote)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "ssh" && endpoint.Scheme != httpsScheme) ||
		endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return fault.New("Git remote must be a remote name, absolute path, SSH URL or HTTPS URL")
	}

	if endpoint.User != nil {
		_, password := endpoint.User.Password()
		if password || endpoint.Scheme == httpsScheme {
			return fault.New("Git remote credentials must be configured outside the URL")
		}
	}

	return nil
}

// validateLocalRemotes keeps path remotes out of state and away from every repository's sources:
// each push writes into the remote, so a watched remote would be recorded again after every publication.
// It runs once all sources are valid; remotes in DB repositories are never published.
func (c *Config) validateLocalRemotes() error {
	reserved := c.InternalPaths()

	for _, name := range c.Names() {
		remote, local := c.pathRemote(name)
		if !local {
			continue
		}

		err := reservedRemote(name, c.remoteLocations(name), reserved)
		if err != nil {
			return err
		}

		err = overlappingRemote(name, remote, c)
		if err != nil {
			return err
		}
	}

	return nil
}

// reservedRemote rejects a path remote that is, or whose trusted target is, inside state.
// Source patterns stay on the path as written: a trusted link that lands in a source is skipped when
// that source is resolved. State is not a source, so nothing later would notice the target.
func reservedRemote(name string, locations, reserved []string) error {
	for _, candidate := range locations {
		if pathutil.Within(reserved, candidate) {
			return fault.New("repository " + name + ": Git remote is inside reserved state: " + candidate)
		}
	}

	return nil
}

func overlappingRemote(name, remote string, cfg *Config) error {
	for _, other := range cfg.Names() {
		for _, source := range cfg.Repositories[other].Paths {
			if sourcesMayOverlap(remote, source) {
				return fault.New("repository " + name + ": Git remote overlaps source: " + remote + " and " + source)
			}
		}
	}

	return nil
}

// validateDistinctRemotes rejects two Git repositories publishing one branch of one URL or path remote:
// their histories are unrelated, so the remote would reject the second one's pushes forever. A remote
// name may point anywhere and is not compared.
func (c *Config) validateDistinctRemotes() error {
	publishers := make(map[[2]string]string)

	for _, name := range c.Names() {
		settings := c.Repositories[name].Storage.Git
		if c.Repositories[name].Type != RepositoryGit || settings.Remote == "" || validObjectName(settings.Remote) {
			continue
		}

		remote, local := c.publishedPath(name)
		if !local {
			remote = settings.Remote
		}

		target := [2]string{remote, settings.Branch}
		if other, exists := publishers[target]; exists {
			return fault.New("repositories " + other + " and " + name + " publish to the same Git branch: " +
				remote + " " + settings.Branch)
		}

		publishers[target] = name
	}

	return nil
}

// remoteLocations is the clean path remote plus the path Git publishes to when a trusted link resolves.
// A missing or untrusted link stays as written: publication already fails those, and following an
// untrusted link here could be aimed at a reserved path to skip this check.
func (c *Config) remoteLocations(name string) []string {
	remote, local := c.pathRemote(name)
	if !local {
		return nil
	}

	locations := []string{remote}

	resolved, err := gitRemote(remote, trustedOwner)
	if err == nil && resolved != remote {
		locations = append(locations, resolved)
	}

	return locations
}

// publishedPath is the path Git pushes to. A trusted link uses its target, so two links to one
// repository collide; a missing or untrusted link keeps the path as written.
func (c *Config) publishedPath(name string) (string, bool) {
	remote, local := c.pathRemote(name)
	if !local {
		return "", false
	}

	resolved, err := gitRemote(remote, trustedOwner)
	if err != nil {
		return remote, true
	}

	return resolved, true
}

// pathRemote returns the clean path remote of a Git repository; other repositories and remotes report false.
func (c *Config) pathRemote(name string) (string, bool) {
	repo := c.Repositories[name]
	if repo.Type != RepositoryGit || !filepath.IsAbs(repo.Storage.Git.Remote) {
		return "", false
	}

	return filepath.Clean(repo.Storage.Git.Remote), true
}

func validBranch(branch string) bool {
	if branch == "" || branch == "@" || branch == "HEAD" || strings.HasPrefix(branch, "-") ||
		strings.HasSuffix(branch, ".") || strings.ContainsAny(branch, ` ~^:?*[\`) {
		return false
	}

	if strings.Contains(branch, "..") || strings.Contains(branch, "@{") ||
		strings.ContainsFunc(branch, unicode.IsControl) {
		return false
	}

	for part := range strings.SplitSeq(branch, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}

	return true
}
