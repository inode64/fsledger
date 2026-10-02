package daemon

import (
	"context"
	"time"
)

const maximumPublishBackoff = 15 * time.Minute

// Publication runs in the repository worker, serialized with commits and maintenance.
func (w *worker) publish(ctx context.Context, now time.Time) {
	settings := w.cfg.Storage.Git
	if w.repo == nil || settings.Remote == "" || w.status.LastCommit == "" || now.Before(w.publishNext) ||
		w.status.LastCommit == w.status.LastPublishedCommit {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, settings.PushTimeout)
	defer cancel()

	err := w.repo.Publish(ctx, settings.Remote, settings.Branch, w.status.LastCommit)
	if err != nil {
		w.publishDelay = max(settings.PushInterval, min(w.publishDelay+w.publishDelay, maximumPublishBackoff))
		w.publishNext = time.Now().Add(w.publishDelay)
		w.status.LastPublishError = err.Error()
		w.logger.Warn("Git publication failed", "error", err)

		return
	}

	w.publishDelay = 0
	w.publishNext = time.Now().Add(settings.PushInterval)
	w.status.LastPublishedCommit = w.status.LastCommit
	w.status.LastPublished = time.Now()
	w.status.LastPublishError = ""
	w.logger.Info("Git publication completed", "branch", settings.Branch, "commit", w.status.LastCommit)
}
