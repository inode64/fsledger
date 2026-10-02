package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/catalog"
)

func TestGitStatusTimeoutDoesNotDiscardCatalogStatus(t *testing.T) {
	t.Parallel()
	runner := makeWorker(t)
	writeSource(t, runner, "file", "observed")

	err := runner.reconcile(t.Context(), "initial")
	if err != nil {
		t.Fatal(err)
	}

	want, err := runner.catalog.Stats(t.Context())
	if err != nil || want.Entries == 0 {
		t.Fatal("missing inventory", want, err)
	}

	index := filepath.Join(runner.repo.Path, ".git", "index")

	err = os.Remove(index)
	if err != nil {
		t.Fatal(err)
	}

	// A FIFO makes the real Git command wait until its status deadline cancels it.
	err = unix.Mkfifo(index, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	runner.gitStatusNext = time.Time{}
	runner.status.Catalog = catalog.Stats{}
	runner.saveStatus(t.Context())

	if !strings.Contains(runner.status.GitStatus, "deadline exceeded") {
		t.Fatal("Git did not reach the status deadline", runner.status.GitStatus)
	}

	if runner.status.Catalog != want {
		t.Fatal("Git timeout discarded independent catalog counters", runner.status.Catalog, want)
	}
}
