package daemon

import (
	"context"
	"errors"
	"log/slog"
	"testing"
)

var errRepositoryLocked = errors.New("repository is in use")

// A failed repository runs again until it starts; one still failing at shutdown is reported.
func TestRetryRepositoryUntilItRuns(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	attempts := 0

	err := retryRepository(ctx, slog.New(slog.DiscardHandler), "repo", func() error {
		attempts++
		if attempts < 2 {
			return errRepositoryLocked
		}

		return nil
	})
	if err != nil || attempts != 2 {
		t.Fatal("failed repository not retried", attempts, err)
	}

	err = retryRepository(ctx, slog.New(slog.DiscardHandler), "repo", func() error {
		cancel()

		return errRepositoryLocked
	})
	if !errors.Is(err, errRepositoryLocked) {
		t.Fatal("failure pending at shutdown not reported", err)
	}
}
