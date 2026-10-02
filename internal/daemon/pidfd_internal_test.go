package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/inode64/fsledger/internal/watcher/fanotify"
)

func TestPIDFDProbeSharesConclusiveResults(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	probe := sharedPIDFDProbe(func(context.Context, string) fanotify.PIDFDLifetime {
		calls.Add(1)

		return fanotify.PIDFDLifetime{Checked: true, Available: false, Reason: ""}
	})

	var wait sync.WaitGroup
	for range 20 {
		wait.Go(func() {
			if result := probe(t.Context(), "runtime"); !result.Checked || result.Available {
				t.Errorf("unexpected shared result: %+v", result)
			}
		})
	}

	wait.Wait()

	if calls.Load() != 1 {
		t.Fatal("kernel probed once per caller", calls.Load())
	}
}

func TestPIDFDProbeRetriesInconclusiveAndCancelledResults(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	calls := 0
	probe := sharedPIDFDProbe(func(context.Context, string) fanotify.PIDFDLifetime {
		calls++
		if calls == 1 {
			return fanotify.PIDFDLifetime{Checked: false, Available: false, Reason: "temporary failure"}
		}

		if calls == 2 {
			cancel()
		}

		return fanotify.PIDFDLifetime{Checked: true, Available: true, Reason: ""}
	})

	probe(ctx, "runtime")
	probe(ctx, "runtime")
	probe(ctx, "runtime")

	result := probe(t.Context(), "runtime")
	if calls != 3 || !result.Checked || !result.Available {
		t.Fatal("inconclusive or cancelled result cached", calls, result)
	}
}

func TestPIDFDWarningIsOnceAndDoesNotDeclareEventLoss(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer

	runner := new(worker)
	runner.logger = slog.New(slog.NewTextHandler(&output, nil))
	// Disabled backends must not probe; even a configuration is unnecessary here.
	runner.checkPIDFDLifetime(t.Context(), false)

	if runner.status.FanotifyPIDFDLifetime != nil || output.Len() != 0 {
		t.Fatal("disabled pidfd attribution was probed")
	}

	runner.recordPIDFDLifetime(fanotify.PIDFDLifetime{Available: false, Checked: true, Reason: ""})
	runner.checkPIDFDLifetime(t.Context(), true)

	if len(runner.status.Warnings) != 1 || strings.Count(output.String(), fanotify.ReapedPIDFDFix) != 1 {
		t.Fatalf("warning missing or repeated: %+v %s", runner.status.Warnings, output.String())
	}

	if runner.pendingError || runner.dirty || runner.forceReconcile {
		t.Fatal("actor limitation incorrectly declared filesystem event loss")
	}
}
