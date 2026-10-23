package config

import (
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/redact"
)

// AI defaults also define the maximum total budget for one commit.
const (
	DefaultAITimeout   = 5 * time.Second
	DefaultAIDiffBytes = 64 << 10
	maxAIDiffBytes     = 1 << 20
)

// AI provider identifiers are shared with the runtime adapters.
const (
	AIProviderOpenAI           = "openai"
	AIProviderOpenAICompatible = "openai-compatible"
	AIProviderAnthropic        = "anthropic"
)

var aiCredentialVariable = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// AIProfile describes a named service; routing belongs to repositories.
type AIProfile struct {
	Provider     string        `yaml:"provider"`
	Endpoint     string        `yaml:"endpoint"`
	Model        string        `yaml:"model"`
	APIKeyEnv    string        `yaml:"api_key_env"`
	Redact       []redact.Rule `yaml:"redact"`
	Timeout      time.Duration `yaml:"timeout"`
	MaxDiffBytes int           `yaml:"max_diff_bytes"`
}

func (loader *catalogLoader) aiProfiles(base string, cfg *Config) error {
	directory := filepath.Join(base, "ai")

	_, err := os.Stat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fault.Wrap("inspect AI catalog", err)
	}

	return loader.catalog(base, []string{directory}, func(name, file string) error {
		if _, exists := cfg.AI[name]; exists {
			return fault.New("duplicate AI profile: " + name)
		}

		profile := AIProfile{
			Provider:     "",
			Endpoint:     "",
			Model:        "",
			APIKeyEnv:    "",
			Redact:       nil,
			Timeout:      DefaultAITimeout,
			MaxDiffBytes: DefaultAIDiffBytes,
		}

		readErr := loader.read(file, &profile)
		if readErr != nil {
			return fault.New("invalid AI profile document: " + name)
		}

		cfg.AI[name] = profile

		return nil
	})
}

func (c *Config) validateAI() error {
	for name, profile := range c.AI {
		if !validObjectName(name) {
			return fault.New("invalid AI profile name")
		}

		err := profile.validate()
		if err != nil {
			return fault.Wrap("AI profile "+name, err)
		}
	}

	for name := range c.Repositories {
		repo := c.Repositories[name]

		err := c.validateAISelection(&repo)
		if err != nil {
			return fault.Wrap("repository "+name, err)
		}
	}

	return nil
}

func (c *Config) validateAISelection(repo *Repository) error {
	if len(repo.IACommit) > 0 && repo.Type != RepositoryGit {
		return fault.New("ia_commit requires Git")
	}

	seen := make(map[string]bool)

	var rules []redact.Rule

	for _, name := range repo.IACommit {
		profile, exists := c.AI[name]
		if !exists || seen[name] {
			return fault.New("unknown or duplicate ia_commit profile: " + name)
		}

		seen[name] = true

		rules = append(rules, profile.Redact...)
	}

	err := ValidateAIIncludes(repo.IAInclude)
	if err != nil {
		return fault.Wrap("ia_include", err)
	}

	_, err = redact.Compile(rules)

	return err
}

func (profile AIProfile) validate() error {
	if !slices.Contains([]string{AIProviderOpenAI, AIProviderOpenAICompatible, AIProviderAnthropic}, profile.Provider) {
		return fault.New("unsupported AI provider")
	}

	if strings.TrimSpace(profile.Model) == "" || profile.Timeout <= 0 || profile.Timeout > DefaultAITimeout ||
		profile.MaxDiffBytes < 1 || profile.MaxDiffBytes > maxAIDiffBytes {
		return fault.New("invalid AI model or limits (timeout must not exceed 5s)")
	}

	err := validateAIEndpoint(profile.Endpoint)
	if err != nil {
		return err
	}

	if profile.APIKeyEnv != "" && !aiCredentialVariable.MatchString(profile.APIKeyEnv) {
		return fault.New("invalid AI credential environment variable")
	}

	_, err = redact.Compile(profile.Redact)

	return err
}

func validateAIEndpoint(raw string) error {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" ||
		endpoint.Fragment != "" {
		return fault.New("invalid AI endpoint")
	}

	address := net.ParseIP(endpoint.Hostname())

	loopback := address != nil && address.IsLoopback()
	if endpoint.Scheme != httpsScheme && (endpoint.Scheme != "http" || !loopback) {
		return fault.New("AI endpoint requires HTTPS; HTTP is allowed only for loopback IPs")
	}

	return nil
}

// AIEnabled requires an explicit provider selection and a nonempty path allowlist.
func (repo *Repository) AIEnabled() bool {
	return len(repo.IACommit) > 0 && len(repo.IAInclude) > 0
}

// ValidateAIIncludes checks an ia_include or reports.ai.include allowlist; the summary adapter shares it.
func ValidateAIIncludes(patterns []string) error {
	for _, pattern := range patterns {
		if !strings.HasPrefix(pattern, "/") {
			return fault.New("AI include patterns must be absolute")
		}
	}

	_, err := exclude.New(patterns...)

	return err
}
