package integrity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/exclude"
)

//nolint:funlen // Ordered assertions exercise admission, cancellation, event progress and permit reuse.
func TestScanAdmissionLeavesFileBudgetAvailable(t *testing.T) {
	t.Parallel()

	scanner := NewScanner(1, 1)
	root := t.TempDir()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)

	var once sync.Once
	defer once.Do(func() { close(release) })

	go func() {
		done <- scanner.ScanPaths(ctx, []string{root}, nil, matcher, "sha256", false, func(Record) error {
			close(entered)

			select {
			case <-release:
			case <-ctx.Done():
			}

			return nil
		})
	}()

	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Admission must not consume the sole observation slot needed by events/prepare.
	_, err = scanner.Observe(ctx, root, "sha256", false)
	if err != nil {
		t.Fatal("event blocked by traversal budget", err)
	}

	waiting, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()

	err = scanner.ScanPaths(
		waiting,
		[]string{root}, nil, matcher,
		"sha256",
		false,
		func(Record) error {
			t.Error("scan exceeded limit")

			return nil
		},
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("admission did not cancel", err)
	}

	once.Do(func() { close(release) })

	err = <-done
	if err != nil {
		t.Fatal(err)
	}
	// A released permit must remain usable, including nested file observation.
	err = scanner.ScanPaths(ctx, []string{root}, nil, matcher, "sha256", false, func(record Record) error {
		_, observeErr := scanner.Observe(ctx, string(record.Path), "sha256", false)

		return observeErr
	})
	if err != nil {
		t.Fatal(err)
	}
}
