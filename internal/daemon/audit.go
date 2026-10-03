package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/inode64/fsledger/internal/pathutil"

	"github.com/inode64/fsledger/internal/attribution/audit"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/exclude"
)

type auditScope struct {
	matcher *exclude.Matcher
	roots   *activeRoots
}

func startAudit(
	ctx context.Context,
	cfg *config.Config,
	logger *slog.Logger,
	matchers map[string]*exclude.Matcher,
	roots map[string]*activeRoots,
) *audit.Provider {
	scopes := make(map[string][]auditScope)

	for _, name := range cfg.Names() {
		settings := cfg.Repositories[name].Attribution.Audit
		if !settings.Enabled || cfg.Repositories[name].Watch.Backend == event.Polling {
			continue
		}

		scopes[settings.Key] = append(
			scopes[settings.Key],
			auditScope{roots: roots[name], matcher: matchers[name]},
		)
	}

	if len(scopes) == 0 {
		return nil
	}

	accept := make(map[string]func(string) bool, len(scopes))
	for key, group := range scopes {
		accept[key] = acceptsAuditScopes(group)
	}

	provider, err := audit.Start(ctx, accept, logger)
	if err != nil {
		logger.Info("Audit attribution unavailable", "reason", err.Error())

		return nil
	}

	logger.Info("passive Audit attribution enabled", "keys", len(accept),
		"coverage", "requires administrator rules; absence of records leaves actor unknown")

	return provider
}

func (w *worker) enrichAudit(ctx context.Context) {
	if w.audit == nil || !w.cfg.Attribution.Audit.Enabled {
		return
	}

	w.manager.Enrich(func(raw event.Raw) event.Actor { return w.audit.Resolve(ctx, raw, w.cfg.Attribution.Audit.Key) })
}

func (w *worker) auditDelay() time.Duration {
	if w.audit == nil || !w.cfg.Attribution.Audit.Enabled {
		return 0
	}

	return audit.CorrelationDelay
}

func acceptsAuditScopes(scopes []auditScope) func(string) bool {
	return func(path string) bool {
		for _, scope := range scopes {
			for _, root := range scope.roots.load() {
				if pathutil.Contains(root, path) && !scope.matcher.Match(path) {
					return true
				}
			}
		}

		return false
	}
}
