package daemon

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/inode64/fsledger/internal/config"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/watcher/fanotify"
)

const (
	statusInterval    = time.Second
	gitStatusInterval = 30 * time.Second
	statusTimeout     = 2 * time.Second
	warningLimit      = 16
)

// Status is an atomic local snapshot; Updated permits stale-daemon detection. Paths are the sources
// selected right now. MissingSources match nothing, SkippedSources must not be watched, and
// UnavailableSources are kept because their filesystem, not the source, disappeared.
type Status struct {
	GitStatusUpdated      time.Time               `json:"git_status_updated"`
	LastPublished         time.Time               `json:"last_published"`
	LastReconciliation    time.Time               `json:"last_reconciliation"`
	LastFetched           time.Time               `json:"last_fetched"`
	Updated               time.Time               `json:"updated"`
	FanotifyPIDFDLifetime *fanotify.PIDFDLifetime `json:"fanotify_pidfd_lifetime,omitempty"`
	RemoteCommit          string                  `json:"remote_commit,omitempty"`
	Repository            string                  `json:"repository"`
	Type                  string                  `json:"type"`
	GitStatus             string                  `json:"git_status"`
	AIStatus              string                  `json:"ai_status,omitempty"`
	InboundApplied        string                  `json:"inbound_applied,omitempty"`
	LastPublishedCommit   string                  `json:"last_published_commit"`
	LastPublishError      string                  `json:"last_publish_error"`
	InboundPending        string                  `json:"inbound_pending,omitempty"`
	LastFetchError        string                  `json:"last_fetch_error,omitempty"`
	SyncConflict          string                  `json:"sync_conflict,omitempty"`
	LastCommit            string                  `json:"last_commit"`
	LastChange            string                  `json:"last_change"`
	SyncBlocked           string                  `json:"sync_blocked,omitempty"`
	SkippedSources        []string                `json:"skipped_sources,omitempty"`
	Paths                 []string                `json:"paths"`
	Backends              []string                `json:"backends"`
	Warnings              []string                `json:"warnings"`
	UnstablePaths         []string                `json:"unstable_paths,omitempty"`
	MissingSources        []string                `json:"missing_sources,omitempty"`
	UnavailableSources    []string                `json:"unavailable_sources,omitempty"`
	Scan                  ScanStatus              `json:"scan"`
	Catalog               catalog.Stats           `json:"catalog"`
	Reports               catalog.ReportStatus    `json:"reports"`
	PendingGroups         int                     `json:"pending_groups"`
	PendingEvents         int                     `json:"pending_events"`
	Running               bool                    `json:"running"`
}

func (w *worker) warn(message string) {
	w.logger.Warn("repository warning", "reason", message)

	w.pendingError = true

	// Reasons travel with the error and recovery alerts and stay in the status until the repository recovers;
	// repeats (one per lost directory) add nothing.
	if !slices.Contains(w.errorReasons, message) && len(w.errorReasons) < warningLimit {
		w.errorReasons = append(w.errorReasons, message)
	}

	w.refreshWarnings()
}

// advise records a permanent limitation (such as the kernel pidfd advisory) that is neither an error nor
// evidence of lost events: it stays in the status across recoveries.
func (w *worker) advise(message string) {
	if !slices.Contains(w.advisories, message) {
		w.advisories = append(w.advisories, message)
	}

	w.refreshWarnings()
}

// refreshWarnings rebuilds the published list: advisories first, then the warnings since the last recovery.
func (w *worker) refreshWarnings() {
	w.status.Warnings = append(append([]string{}, w.advisories...), w.errorReasons...)
}

func (w *worker) saveStatus(ctx context.Context) {
	w.status.Updated = time.Now()
	w.status.UnstablePaths = slices.Sorted(maps.Keys(w.unstable))

	w.status.PendingGroups, w.status.PendingEvents = w.manager.Pending()
	for _, detector := range w.watchers {
		w.status.PendingEvents += len(detector.Events())
	}

	w.updateGitStatus(ctx)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statusTimeout)
	defer cancel()

	stats, err := w.catalog.Stats(ctx)
	if err == nil {
		w.status.Catalog = stats
		w.status.Scan.Initialized = stats.LastFullScan != 0
	} else {
		w.logger.Warn("catalog status unavailable", "error", err)
	}

	if !w.status.Scan.Initialized || w.pendingError || len(w.status.UnavailableSources) > 0 ||
		len(w.status.SkippedSources) > 0 ||
		len(w.unstable) > 0 {
		gapErr := w.catalog.ReportGap(ctx)
		if gapErr != nil {
			w.logger.Warn("cannot record report coverage gap", "error", gapErr)
		}
	}

	w.status.Reports = w.catalog.Reports()

	data, err := json.MarshalIndent(w.status, "", "  ")
	if err != nil {
		w.logger.Warn("encode status", "error", err)

		return
	}

	err = writeStatus(w.cfg.Runtime, w.name+".json", data)
	if err != nil {
		w.logger.Warn("write status", "error", err)
	}
}

// Git may exhaust its deadline; catalog counters and coverage need their own budget.
func (w *worker) updateGitStatus(ctx context.Context) {
	if w.repo == nil || time.Now().Before(w.gitStatusNext) {
		return
	}

	w.gitStatusNext = time.Now().Add(gitStatusInterval)
	w.status.GitStatusUpdated = time.Now()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statusTimeout)
	defer cancel()

	status, err := w.repo.Status(ctx)
	if err != nil {
		w.status.GitStatus = err.Error()
	} else {
		w.status.GitStatus = status
	}
}

func (w *worker) announceError(ctx context.Context) {
	if !w.pendingError || w.announcedError {
		return
	}

	err := w.catalog.Announce(ctx, config.NotificationError, strings.Join(w.errorReasons, "; "))
	if err != nil {
		w.logger.Warn("cannot queue repository error", "error", err)

		return
	}

	w.announcedError = true
}
