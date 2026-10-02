package config

import (
	"path/filepath"
	"time"

	"github.com/inode64/fsledger/internal/filter"
)

// RepositoryGit and RepositoryDB select independent persistence adapters.
const (
	CatalogDirectory  = "catalog.pebble"
	ReferenceBaseline = "baseline"
	ReferencePrevious = "previous"
	RepositoryGit     = "git"
	RepositoryDB      = "db"
)

// Paths discovers named configuration objects.
type Paths struct {
	Templates    string   `yaml:"templates"`
	Repositories []string `yaml:"repositories"`
	Notifiers    []string `yaml:"notifiers"`
	Excludes     []string `yaml:"excludes"`
}

// Template defines the independently rendered parts of a notification.
type Template struct {
	Subject string `yaml:"subject"`
	Body    string `yaml:"body"`
}

// Scan limits concurrent filesystem work across repositories.
type Scan struct {
	Workers       int `yaml:"workers"`
	MaxConcurrent int `yaml:"max_concurrent"`
}

// Storage locates durable state; Path is a root shared by named repositories.
type Storage struct {
	Path string     `yaml:"path"`
	Git  GitStorage `yaml:"git"`
}

// GitStorage bounds each bulk Git command, including administrator hooks.
type GitStorage struct {
	Remote        string        `yaml:"remote"`
	Branch        string        `yaml:"branch"`
	FetchInterval time.Duration `yaml:"fetch_interval"`
	FetchTimeout  time.Duration `yaml:"fetch_timeout"`
	PushInterval  time.Duration `yaml:"push_interval"`
	PushTimeout   time.Duration `yaml:"push_timeout"`
	Timeout       time.Duration `yaml:"timeout"`
	Bidirectional bool          `yaml:"bidirectional"`
}

// IntegrityFields are the names accepted in integrity.compare; the integrity package compares each of them.
//
//nolint:gochecknoglobals // Immutable vocabulary shared with the comparison tests.
var IntegrityFields = []string{
	"hash", "type", "mode", "uid", "gid", "size", "mtime", "ctime", "atime", "btime",
	"xattrs", "acl", "inode", "device", "nlink", "target",
}

// Integrity selects the approved reference and metadata comparison policy.
type Integrity struct {
	Reference string   `yaml:"reference"`
	Compare   []string `yaml:"compare"`
	Hash      Hash     `yaml:"hash"`
}

// Hash algorithm names are shared by policy validation and digest selection.
const (
	HashSHA256    = "sha256"
	HashSHA512    = "sha512"
	HashSHA512256 = "sha512_256"
	HashSHA3256   = "sha3_256"
	HashSHA3512   = "sha3_512"
	HashBLAKE3    = "blake3"
	HashXXHash64  = "xxhash64"
)

// Hash controls content verification independently of metadata timestamps.
type Hash struct {
	FullScanSchedule string        `yaml:"full_scan_schedule"`
	FullScanTimezone string        `yaml:"full_scan_timezone"`
	Algorithm        string        `yaml:"algorithm"`
	FullScanInterval time.Duration `yaml:"full_scan_interval"`
	OnEvent          bool          `yaml:"on_event"`
}

// Notification event names are persisted in the outbox and exposed in configuration.
const (
	NotificationChange             = "change"
	NotificationIntegrityViolation = "integrity_violation"
	NotificationError              = "error"
	NotificationRecovery           = "recovery"
)

// Notifications selects named destinations and durable delivery policy.
type Notifications struct {
	Ignore         []filter.Rule `yaml:"ignore"`
	Use            []string      `yaml:"use"`
	Events         []string      `yaml:"events"`
	BatchWindow    time.Duration `yaml:"batch_window"`
	MaxBatchWindow time.Duration `yaml:"max_batch_window"`
	MaxPending     int           `yaml:"max_pending"`
}

// NotifierEmail and NotifierSlack select the configured transport adapter.
const (
	NotifierEmail = "email"
	NotifierSlack = "slack"
)

// Notifier defines one transport; its filename supplies the identifier.
type Notifier struct {
	Type     string   `json:"type"     yaml:"type"`
	DSN      string   `json:"-"        yaml:"dsn"`
	URL      string   `json:"-"        yaml:"url"`
	From     string   `json:"from"     yaml:"from"`
	Template string   `json:"template" yaml:"template"`
	To       []string `json:"to"       yaml:"to"`
}

// RepositoryPath returns the effective durable directory of a named repository.
func (c *Config) RepositoryPath(name string) string {
	return filepath.Join(c.Repositories[name].Storage.Path, name)
}

// CatalogPath locates metadata outside the Git worktree or in a DB repository.
func (c *Config) CatalogPath(name string) string {
	root := c.RepositoryPath(name)
	if c.Repositories[name].Type == RepositoryGit {
		return filepath.Join(root, ".git", "fsledger", CatalogDirectory)
	}

	return filepath.Join(root, CatalogDirectory)
}
