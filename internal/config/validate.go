package config

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/inode64/fsledger/internal/pathutil"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/filter"
)

const maxScanConcurrency = 256

const namePattern = `^[a-zA-Z0-9][a-zA-Z0-9._-]*$`

var (
	objectNamePattern = regexp.MustCompile(namePattern)
	auditKeyPattern   = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,31}$`)
)

func validObjectName(name string) bool { return objectNamePattern.MatchString(name) }

// Validate checks logical conflicts without reading disk. CheckPaths inspects log and state locations,
// and ResolveSources reads which sources currently exist.
func (c *Config) Validate() error {
	for _, check := range []func() error{
		c.validateGlobals, c.validateLogging, c.validateOptions, c.validateNotifiers, c.validateAI, c.validateReports,
	} {
		err := check()
		if err != nil {
			return err
		}
	}

	if len(c.Repositories) == 0 {
		return fault.New("at least one repository is required")
	}

	err := c.validateStores()
	if err != nil {
		return err
	}

	// A Git copy and a DB inventory may cover the same files: one keeps content, the other verifies it,
	// and each owns its state. Two repositories of one type would only duplicate work.
	seen := make(map[string][]string)

	for _, name := range c.Names() {
		kind := c.Repositories[name].Type

		err = c.validateRepository(name, seen[kind])
		if err != nil {
			return err
		}

		seen[kind] = append(seen[kind], c.Repositories[name].Paths...)
	}

	return nil
}

// validateStores keeps repository state apart. Sources are kept out of it through InternalPaths,
// which holds every storage root these directories live under.
func (c *Config) validateStores() error {
	var stores []string

	for _, name := range c.Names() {
		root := filepath.Join(c.Repositories[name].Storage.Path, name)
		for _, previous := range stores {
			if Overlaps(root, previous) {
				return fault.New("repository storage overlaps: " + root)
			}
		}

		stores = append(stores, root)
	}

	return nil
}

func (c *Config) validateRepository(name string, seen []string) error {
	if !validObjectName(name) {
		return fault.New("invalid repository name: " + name)
	}

	repo := c.Repositories[name]
	if repo.Storage.Git.Bidirectional && repo.Type != RepositoryGit {
		return fault.New("bidirectional synchronization requires a Git repository: " + name)
	}

	if repo.Type != RepositoryGit && repo.Type != RepositoryDB {
		return fault.New("invalid repository type: " + name)
	}

	effective := c.ForRepository(name)
	for _, check := range []func() error{effective.validateOptions, effective.validateLogging} {
		err := check()
		if err != nil {
			return fault.Wrap("repository "+name, err)
		}
	}

	if len(repo.Paths) == 0 {
		return fault.New("repository has no paths: " + name)
	}

	_, err := c.Matcher(name)
	if err != nil {
		return err
	}

	for index, path := range repo.Paths {
		err := effective.validateSource(path, seen, repo.Paths[:index])
		if err != nil {
			return err
		}
	}

	return nil
}

func separatePaths(path string, reserved []string) error {
	for _, other := range reserved {
		if other != "" && Overlaps(path, other) {
			return fault.New("overlapping paths: " + path + " and " + other)
		}
	}

	return nil
}

func (c *Config) validateOptions() error {
	for _, rules := range [][]filter.Rule{c.Commit.Defer, c.Notifications.Ignore} {
		_, err := filter.Compile(rules)
		if err != nil {
			return err
		}
	}

	for _, check := range []func() error{
		c.validateLocations, c.validateWatching, c.validateIntegrity, c.validateNotifications, c.validatePublishing,
		c.Reports.validate,
	} {
		err := check()
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Config) validateGlobals() error {
	if !validObjectName(c.Server.Name) {
		return fault.New("invalid server name")
	}

	if c.Scan.MaxConcurrent < 1 || c.Scan.MaxConcurrent > maxScanConcurrency {
		return fault.New("scan.max_concurrent must be between 1 and 256")
	}

	if c.Scan.Workers < 1 || c.Scan.Workers > maxScanConcurrency {
		return fault.New("scan.workers must be between 1 and 256")
	}

	return CleanAbsolute(c.Runtime)
}

func (c *Config) validateLocations() error {
	err := CleanAbsolute(c.Storage.Path)
	if err != nil {
		return err
	}

	if Overlaps(c.Storage.Path, c.Runtime) {
		return fault.New("storage and runtime must be separate non-overlapping directories")
	}

	if c.Storage.Git.Timeout <= 0 {
		return fault.New("storage.git.timeout must be positive")
	}

	return nil
}

func (c *Config) validateWatching() error {
	if c.Attribution.Audit.Enabled &&
		!auditKeyPattern.MatchString(c.Attribution.Audit.Key) {
		return fault.New("Audit key must contain 1-31 letters, digits, dots, underscores or hyphens")
	}

	switch c.Watch.Backend {
	case BackendAuto, event.Fanotify, event.Inotify, event.Polling:
	default:
		return fault.New("invalid backend: " + c.Watch.Backend)
	}

	if c.Commit.Debounce <= 0 || c.Commit.MaxDelay < c.Commit.Debounce {
		return fault.New("durations must be positive and max_delay >= debounce")
	}

	if c.Watch.Backend != event.Polling && (!c.Watch.Reconcile.OnStart || !c.Watch.Reconcile.OnStop) {
		return fault.New("disabling watch.reconcile.on_start or on_stop requires polling")
	}

	return validateScanSchedule(c.Watch.Reconcile.Interval, c.Watch.Reconcile.Schedule, c.Watch.Reconcile.Timezone)
}

func (c *Config) validateIntegrity() error {
	if c.Integrity.Reference != ReferenceBaseline && c.Integrity.Reference != ReferencePrevious {
		return fault.New("integrity.reference must be baseline or previous")
	}

	if !slices.Contains(
		[]string{HashSHA256, HashSHA512, HashSHA512256, HashSHA3256, HashSHA3512, HashBLAKE3, HashXXHash64},
		c.Integrity.Hash.Algorithm,
	) {
		return fault.New("unsupported hash algorithm")
	}

	if c.Integrity.Hash.Algorithm == HashXXHash64 && c.Integrity.Reference == ReferenceBaseline {
		return fault.New("baseline integrity requires a cryptographic hash")
	}

	err := validateScanSchedule(c.Integrity.Hash.FullScanInterval,
		c.Integrity.Hash.FullScanSchedule, c.Integrity.Hash.FullScanTimezone)
	if err != nil {
		return fault.Wrap("hash full scan schedule", err)
	}

	for _, field := range c.Integrity.Compare {
		if !slices.Contains(IntegrityFields, field) {
			return fault.New("unknown integrity field: " + field)
		}
	}

	return nil
}

func (c *Config) validateNotifications() error {
	if c.Notifications.BatchWindow < 0 ||
		c.Notifications.MaxBatchWindow < c.Notifications.BatchWindow ||
		c.Notifications.MaxPending < 1 {
		return fault.New("invalid notification batching or queue limit")
	}

	seen := make(map[string]bool, len(c.Notifications.Use))
	for _, name := range c.Notifications.Use {
		if seen[name] {
			return fault.New("duplicate notifier: " + name)
		}

		seen[name] = true
		if _, ok := c.Notifiers[name]; !ok {
			return fault.New("unknown notifier: " + name)
		}
	}

	for _, name := range c.Notifications.Events {
		if !slices.Contains(
			[]string{NotificationChange, NotificationIntegrityViolation, NotificationError, NotificationRecovery},
			name,
		) {
			return fault.New("unknown notification event: " + name)
		}
	}

	return nil
}

func (c *Config) validateSource(path string, foreign, own []string) error {
	err := CleanAbsolute(path)
	if err != nil {
		return err
	}

	err = validateSourcePattern(path)
	if err != nil {
		return err
	}

	if path == "/" {
		return fault.New("cannot watch / directly; configure its subdirectories")
	}

	if pathutil.HasGitComponent(path) {
		return fault.New("cannot watch Git metadata: " + path)
	}

	err = outsideState(path, c.InternalPaths())
	if err != nil {
		return err
	}

	err = separatePaths(path, []string{c.Logging.File})
	if err != nil {
		return err
	}

	for _, other := range foreign {
		if sourcesMayOverlap(path, other) {
			return fault.New("overlapping paths: " + path + " and " + other)
		}
	}
	// Inside one repository a pattern may select below another entry; resolution keeps the outer root.
	for _, other := range own {
		if !SourcePattern(path) && !SourcePattern(other) && Overlaps(path, other) {
			return fault.New("overlapping paths: " + path + " and " + other)
		}
	}

	return nil
}

// validateSourcePattern bounds patterns to single components below a literal first directory.
func validateSourcePattern(path string) error {
	if !SourcePattern(path) {
		return nil
	}

	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if !doublestar.ValidatePattern(path) || strings.ContainsAny(path, "{}") ||
		slices.Contains(parts, "**") || SourcePattern(parts[0]) {
		return fault.New("unsupported source pattern: " + path)
	}

	return nil
}

// sourcesMayOverlap compares component by component; two patterns are assumed to match each other.
func sourcesMayOverlap(first, second string) bool {
	left := strings.Split(first, "/")
	right := strings.Split(second, "/")

	for index := range min(len(left), len(right)) {
		if !componentsMayMatch(left[index], right[index]) {
			return false
		}
	}

	return true
}

// SourceSelects reports lexically, without reading disk, whether a configured entry covers a path.
func SourceSelects(entry, path string) bool {
	left := strings.Split(entry, "/")
	right := strings.Split(path, "/")

	if len(left) > len(right) {
		return false
	}

	for index, pattern := range left {
		matched, err := doublestar.Match(pattern, right[index])
		if err != nil || !matched {
			return false
		}
	}

	return true
}

func componentsMayMatch(first, second string) bool {
	switch {
	case SourcePattern(first) && SourcePattern(second):
		return true
	case SourcePattern(first):
		matched, err := doublestar.Match(first, second)

		return err != nil || matched
	case SourcePattern(second):
		return componentsMayMatch(second, first)
	default:
		return first == second
	}
}

func outsideState(path string, reserved []string) error {
	for _, state := range reserved {
		if state != "" && pathutil.Contains(state, path) {
			return fault.New("source is inside reserved state: " + path)
		}
	}

	return nil
}
