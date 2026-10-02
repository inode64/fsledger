// Package config loads and validates fsledger configuration without side effects.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"time"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/filter"

	"go.yaml.in/yaml/v3"

	"github.com/inode64/fsledger/internal/exclude"
)

// Config describes independent repository pipelines.
type Config struct {
	Repositories  map[string]Repository `yaml:"-"`
	Notifiers     map[string]Notifier   `yaml:"-"`
	Templates     map[string]Template   `yaml:"-"`
	AI            map[string]AIProfile  `yaml:"-"`
	Logging       Logging               `yaml:"logging"`
	Runtime       string                `yaml:"runtime"`
	Server        Server                `yaml:"server"`
	Paths         Paths                 `yaml:"paths"`
	Exclude       []string              `yaml:"exclude"`
	Attribution   Attribution           `yaml:"attribution"`
	Integrity     Integrity             `yaml:"integrity"`
	Watch         Watch                 `yaml:"watch"`
	Notifications Notifications         `yaml:"notifications"`
	Reports       Reports               `yaml:"reports"`
	Commit        Commit                `yaml:"commit"`
	Storage       Storage               `yaml:"storage"`
	Scan          Scan                  `yaml:"scan"`
}

// Logging selects daemon severity and an optional append-only log file.
type Logging struct {
	Level LogLevel `yaml:"level"`
	File  string   `yaml:"file"`
}

// Attribution configures optional, passive actor sources.
type Attribution struct {
	Audit Audit `yaml:"audit"`
}

// Audit never installs rules; Key selects rules configured by the administrator.
type Audit struct {
	Key     string `yaml:"key"`
	Enabled bool   `yaml:"enabled"`
}

// Server names the host recorded in Git commits.
type Server struct {
	Name string `yaml:"name"`
}

// Watch selects event detection and periodic consistency checks.
type Watch struct {
	Backend   string    `yaml:"backend"`
	Reconcile Reconcile `yaml:"reconcile"`
}

// Reconcile configures the maximum ordinary polling interval.
type Reconcile struct {
	Schedule string        `yaml:"schedule"`
	Timezone string        `yaml:"timezone"`
	Interval time.Duration `yaml:"interval"`
	OnStart  bool          `yaml:"on_start"`
	OnStop   bool          `yaml:"on_stop"`
	Enabled  bool          `yaml:"enabled"`
}

// Commit controls batching and observed-identity grouping.
type Commit struct {
	Defer    []filter.Rule `yaml:"defer"`
	Grouping Grouping      `yaml:"grouping"`
	Debounce time.Duration `yaml:"debounce"`
	MaxDelay time.Duration `yaml:"max_delay"`
}

// Grouping controls the observed identity used to batch changes.
type Grouping struct {
	ByProcess      bool `yaml:"by_process"`
	ByUser         bool `yaml:"by_user"`
	PreferLoginUID bool `yaml:"prefer_login_uid"`
}

// Repository owns source paths and additional exclusions. Paths are literal paths or single-component
// patterns describing what may exist on disk; ResolveSources gives their current meaning. A Git and a
// DB repository may select the same sources, two repositories of one type may not.
type Repository struct {
	Type          string        `yaml:"type"`
	IACommit      []string      `yaml:"ia_commit"`
	IAInclude     []string      `yaml:"ia_include"`
	Attribution   Attribution   `yaml:"attribution"`
	Paths         []string      `yaml:"paths"`
	Exclude       []string      `yaml:"exclude"`
	Integrity     Integrity     `yaml:"integrity"`
	Watch         Watch         `yaml:"watch"`
	Notifications Notifications `yaml:"notifications"`
	Reports       Reports       `yaml:"reports"`
	Commit        Commit        `yaml:"commit"`
	Storage       Storage       `yaml:"storage"`
}

// ForRepository returns an independent effective configuration for one worker.
func (c *Config) ForRepository(name string) *Config {
	resolved := *c
	repo := c.Repositories[name]
	resolved.Watch, resolved.Attribution, resolved.Storage = repo.Watch, repo.Attribution, repo.Storage
	resolved.Integrity, resolved.Notifications = repo.Integrity, repo.Notifications
	resolved.Commit = repo.Commit
	resolved.Reports = repo.Reports

	return &resolved
}

// Names returns deterministic repository order.
func (c *Config) Names() []string {
	return repositoryNames(c.Repositories)
}

func repositoryNames(repositories map[string]Repository) []string {
	names := make([]string, 0, len(repositories))
	for name := range repositories {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// Matcher combines exclusions additively.
func (c *Config) Matcher(name string) (*exclude.Matcher, error) {
	matcher, err := exclude.New(append(append([]string(nil), c.Exclude...), c.Repositories[name].Exclude...)...)
	if err != nil {
		return nil, err
	}

	return matcher.Protect(c.InternalPaths()...), nil
}

func decodeDocument(reader io.Reader, target any, limit int) (int, error) {
	data, readErr := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if readErr != nil {
		return 0, fmt.Errorf("read configuration: %w", readErr)
	}

	if len(data) > limit {
		return 0, fault.New("configuration exceeds 4 MiB")
	}

	size := len(data)

	data, expandErr := expandHost(data)
	if expandErr != nil {
		return 0, expandErr
	}

	if len(data) > limit {
		return 0, fault.New("expanded configuration exceeds remaining 4 MiB budget")
	}

	size = max(size, len(data))

	resetErr := resetScanSchedules(data, target)
	if resetErr != nil {
		return 0, resetErr
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	err := decoder.Decode(target)
	if err != nil {
		return 0, fmt.Errorf("decode configuration: %w", err)
	}

	var extra any

	err = decoder.Decode(&extra)
	if !errors.Is(err, io.EOF) {
		return 0, fault.New("configuration must contain one YAML document")
	}

	return size, nil
}

// InternalPaths reserves literal state directories of every loaded repository.
func (c *Config) InternalPaths() []string {
	paths := slices.Grow([]string{c.Storage.Path, c.Runtime}, len(c.Repositories))
	for name := range c.Repositories {
		paths = append(paths, c.Repositories[name].Storage.Path)
	}

	slices.Sort(paths)

	return slices.Compact(paths)
}

func (c *Config) repositoryDefaults() Repository {
	return Repository{
		IACommit: nil, IAInclude: nil,
		Type: RepositoryGit, Watch: c.Watch, Attribution: c.Attribution,
		Commit: c.Commit, Storage: c.Storage, Integrity: c.Integrity, Notifications: c.Notifications,
		Reports: c.Reports,
		Paths:   nil, Exclude: nil,
	}
}
