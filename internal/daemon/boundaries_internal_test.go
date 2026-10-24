package daemon

import (
	"testing"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/mountinfo"
)

func TestBoundaryMatcherUsesLiteralPathsAndPreservesBase(t *testing.T) {
	t.Parallel()

	matcher, err := exclude.New("/root/cache/**")
	if err != nil {
		t.Fatal(err)
	}

	runner := new(worker)
	runner.matcher = matcher

	scoped := runner.boundaryMatcher(
		"/root",
		[]mountinfo.Mount{{Point: "/root/disk[1]", Type: "test", Device: "", ID: 0}},
	)
	if !scoped.Match("/root/disk[1]/file") || scoped.Match("/root/disk1/file") || !scoped.Match("/root/cache/file") {
		t.Fatal("nested mount boundary was interpreted as a glob or base exclusions were lost")
	}

	if matcher.Match("/root/disk[1]/file") {
		t.Fatal("scoping mutated the shared matcher")
	}
}

// A remote filesystem, such as a VM's virtiofs share, raises no event for changes made elsewhere: a lasting
// property that forces reconciliation, not an error to announce and recover from on every start.
func TestRemoteFilesystemForcesReconciliationWithoutError(t *testing.T) {
	t.Parallel()

	runner := makeWorker(t)
	runner.cfg.Watch.Backend = event.Polling
	path := t.TempDir()

	runner.startBoundary(t.Context(), path, mountinfo.Mount{Point: path, Type: "virtiofs"}, runner.matcher)

	if !runner.forceReconcile || runner.pendingError || len(runner.status.Warnings) != 0 {
		t.Fatal("remote filesystem reported as an error or left without reconciliation", runner.status.Warnings)
	}
}
