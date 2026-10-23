package daemon

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
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

// The delay doubles while a repository keeps failing, and starts over after a run that outlasted the longest one.
func TestRepositoryRetryDelayStartsOverAfterAHealthyRun(t *testing.T) {
	t.Parallel()

	for _, test := range []struct{ delay, ran, want time.Duration }{
		{0, 0, repositoryRetryDelay},
		{repositoryRetryDelay, time.Second, 2 * repositoryRetryDelay},
		{4 * time.Minute, 0, maximumRepositoryRetryDelay},
		{maximumRepositoryRetryDelay, time.Minute, maximumRepositoryRetryDelay},
		{maximumRepositoryRetryDelay, maximumRepositoryRetryDelay, repositoryRetryDelay},
	} {
		if got := nextRepositoryRetry(test.delay, test.ran); got != test.want {
			t.Errorf("after %v and a %v run: %v, want %v", test.delay, test.ran, got, test.want)
		}
	}
}
