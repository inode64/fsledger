package config

import "time"

// DefaultPath is shared by the foreground CLI and daemon entry point.
const DefaultPath = "/etc/fsledger/fsledger.yaml"

// DefaultGitTimeout bounds bulk Git commands while permitting large snapshots.
const DefaultGitTimeout = 10 * time.Minute

// DefaultPushTimeout bounds the pause for network publication in each worker.
const DefaultPushTimeout = 30 * time.Second

// BackendAuto selects the best usable native backend with fallbacks.
const BackendAuto = "auto"

const (
	maxConfigBytes           = 4 << 20
	defaultReconcileInterval = 5 * time.Minute
	defaultDebounce          = 2 * time.Second
	defaultMaxDelay          = 30 * time.Second
)

// defaults returns fresh settings so parsing never mutates shared maps or slices.
func defaults() *Config {
	cfg := new(Config)
	cfg.Runtime = "/run/fsledger"
	cfg.Scan.Workers = 2
	cfg.Reports.Schedule = "0 8 * * *"
	cfg.Reports.Timezone = defaultTimezone
	cfg.Reports.Format = "detailed"
	cfg.Reports.AI.Input = "metadata"
	cfg.Scan.MaxConcurrent = 2
	cfg.Storage.Git.Timeout = DefaultGitTimeout
	cfg.Storage.Git.Branch = "main"
	cfg.Storage.Git.PushInterval = time.Minute
	cfg.Storage.Git.PushTimeout = DefaultPushTimeout
	cfg.Storage.Git.FetchInterval = time.Minute
	cfg.Storage.Git.FetchTimeout = DefaultPushTimeout
	cfg.Integrity = Integrity{
		Reference: ReferenceBaseline,
		Compare:   []string{"hash", "type", "mode", "uid", "gid", "size", "mtime", "ctime", "xattrs", "acl"},
		Hash: Hash{
			Algorithm: HashSHA256, OnEvent: true, FullScanInterval: defaultFullScanInterval,
			FullScanSchedule: "", FullScanTimezone: defaultTimezone,
		},
	}
	cfg.Notifications = Notifications{
		Ignore: nil,
		Use:    nil,
		Events: []string{
			NotificationChange,
			NotificationIntegrityViolation,
			NotificationError,
			NotificationRecovery,
		},
		BatchWindow:    defaultBatchWindow,
		MaxBatchWindow: defaultMaxBatchWindow,
		MaxPending:     defaultMaxPending,
	}
	cfg.Server.Name = "localhost"
	cfg.Storage.Path = "/var/lib/fsledger/repos"
	cfg.Logging.Level = LogLevelInfo
	cfg.Watch.Backend = BackendAuto
	cfg.Watch.Reconcile.Enabled = true
	cfg.Watch.Reconcile.OnStart = true
	cfg.Watch.Reconcile.OnStop = true
	cfg.Watch.Reconcile.Timezone = defaultTimezone
	cfg.Watch.Reconcile.Interval = defaultReconcileInterval
	cfg.Commit.Debounce = defaultDebounce
	cfg.Commit.MaxDelay = defaultMaxDelay
	cfg.Commit.Grouping = Grouping{
		ByProcess:      true,
		ByUser:         true,
		PreferLoginUID: true,
	}
	cfg.Attribution.Audit = Audit{Enabled: true, Key: "fsledger"}

	return cfg
}

const (
	defaultTimezone         = "UTC"
	defaultFullScanInterval = 24 * time.Hour
	defaultBatchWindow      = 30 * time.Second
	defaultMaxBatchWindow   = 15 * time.Minute
	defaultMaxPending       = 10000
)
