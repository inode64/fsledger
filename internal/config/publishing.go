package config

import (
	"net/url"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/inode64/fsledger/internal/fault"
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
