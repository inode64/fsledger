package daemon

import (
	"slices"
	"testing"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/watcher"
)

func TestReturnedSourceRebindSurvivesLaterRefresh(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	runner.cfg.Watch.Backend = event.Inotify
	runner.queue = watcher.NewQueue(8)
	root := runner.roots[0]
	runner.status.UnavailableSources = []string{root}

	if !runner.refreshSources() {
		t.Fatal("returned source did not schedule watcher and identity refresh")
	}

	// The mount returned during a retry backoff, so another refresh runs before applySources.
	if !runner.refreshSources() || !runner.sourcesPending || !slices.Equal(runner.nextRoots, runner.roots) {
		t.Fatal("later refresh dropped the rebind of a returned source")
	}

	runner.applySources(t.Context())

	defer runner.stopWatching()

	if runner.refreshSources() || runner.sourcesPending || runner.nextRoots != nil {
		t.Fatal("applied rebind stayed pending")
	}
}
