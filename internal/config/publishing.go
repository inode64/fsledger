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
		repo := c.Repositories[name]
		if repo.Type != RepositoryGit || !filepath.IsAbs(repo.Storage.Git.Remote) {
			continue
		}

		remote := filepath.Clean(repo.Storage.Git.Remote)
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
