package proc

import (
	"context"
	"time"

	"github.com/inode64/fsledger/internal/event"
)

// ResolveAt enriches a historical observation only if the current process predates it.
// The extra second accounts for /proc/stat's whole-second boot timestamp.
func (provider Provider) ResolveAt(ctx context.Context, raw event.Raw, observedAt time.Time) event.Actor {
	return provider.resolve(ctx, raw, &observedAt)
}
