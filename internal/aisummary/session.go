package aisummary

import (
	"context"
	"time"

	"github.com/inode64/fsledger/internal/config"
)

// acquire bounds preparation and provider attempts together. A nil release means
// the shared budget is occupied; callers skip optional AI rather than wait.
func (session *Session) acquire(ctx context.Context) (context.Context, context.CancelFunc) {
	select {
	case session.manager.slots <- struct{}{}:
	default:
		return ctx, nil
	}

	ctx, cancel := context.WithTimeout(ctx, config.DefaultAITimeout)

	return ctx, func() {
		cancel()
		<-session.manager.slots
	}
}

// tryProviders shares timeouts and cooldown handling while callers retain their
// separate input preparation, redaction and local-result policies.
func (session *Session) tryProviders(
	ctx context.Context,
	attempt func(context.Context, string) (string, string, error),
) (string, string) {
	for _, name := range session.names {
		if ctx.Err() != nil {
			return "", statusTimeout
		}

		if time.Now().Before(session.cooldowns[name]) {
			continue
		}

		provider, cancel := context.WithTimeout(ctx, session.manager.profiles[name].Timeout)
		text, status, err := attempt(provider, name)

		cancel()

		if err == nil {
			return text, status
		}

		session.cooldowns[name] = time.Now().Add(cooldown)
	}

	return "", statusUnavailable
}
