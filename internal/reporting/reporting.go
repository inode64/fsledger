// Package reporting prepares durable scheduled reports independently of repository commits.
package reporting

import (
	"context"
	"log/slog"
	"time"

	"github.com/inode64/fsledger/internal/aisummary"
	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/config"
)

// Run closes due periods and queues frozen parts. SMTP retries never regenerate their summaries.
func Run(
	ctx context.Context,
	store *catalog.Store,
	settings config.Reports,
	manager *aisummary.Manager,
	logger *slog.Logger,
) {
	if !settings.Enabled {
		return
	}

	var session *aisummary.Session

	if settings.AI.Enabled {
		var err error

		session, err = manager.ReportSession(settings.AI)
		if err != nil {
			logger.Warn("report AI unavailable", "error", err)
		}
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		err := prepare(ctx, store, session, settings.AI.Enabled)
		if err != nil && ctx.Err() == nil {
			logger.Warn("report preparation deferred", "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type reportStore interface {
	NextReport(ctx context.Context) (*catalog.ReportDraft, error)
	PublishReport(ctx context.Context, identifier, summary, status string) error
}

func prepare(ctx context.Context, store reportStore, session *aisummary.Session, aiEnabled bool) error {
	draft, err := store.NextReport(ctx)
	if err != nil || draft == nil {
		return err
	}

	summary, status := "", "disabled"
	if aiEnabled {
		status = "unavailable"
	}

	if session != nil {
		summary, status = session.SummarizeReport(ctx, draft.Message.Report.Items)
	}

	return store.PublishReport(ctx, draft.Message.ChangeID, summary, status)
}
