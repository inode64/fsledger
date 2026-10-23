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

		if pathutil.Within(reserved, remote) {
			return fault.New("repository " + name + ": Git remote is inside reserved state: " + remote)
		}

		for _, other := range c.Names() {
			for _, source := range c.Repositories[other].Paths {
				if sourcesMayOverlap(remote, source) {
					return fault.New("repository " + name + ": Git remote overlaps source: " +
						remote + " and " + source)
				}
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

		remote, local := c.pathRemote(name)
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
