package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// incomingPathLimit mirrors the changed path limit enforced by inbound.Prepare.
const incomingPathLimit = 4096

// incomingOverLimit pushes a remote commit that Prepare refuses before any source mutation.
func incomingOverLimit(t *testing.T, runner *worker, editor, directory string) string {
	t.Helper()

	path := filepath.Join(editor, directory)

	err := os.MkdirAll(path, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	for index := range incomingPathLimit {
		err = os.WriteFile(filepath.Join(path, strconv.Itoa(index)), []byte("remote"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	// The edited file is the changed path beyond the limit.
	return incomingEdit(t, runner, editor, "a", "rejected bytes")
}

func TestIncomingRejectedTargetIsNotRetried(t *testing.T) {
	t.Parallel()
	runner, _, editor := incomingWorker(t)
	directory := filepath.Join(strings.TrimPrefix(runner.roots[0], "/"), "many")
	rejected := incomingOverLimit(t, runner, editor, directory)
	fetched := time.Now()

	runner.syncRemote(t.Context(), fetched)

	pending, err := runner.catalog.Incoming(t.Context())
	if err != nil || pending == nil || pending.Target != rejected || !strings.Contains(pending.Conflict, "4096") {
		t.Fatal("path limit conflict not persisted for its target", pending, err, runner.status)
	}

	// A restart reloads the record with an empty status: the same target is neither reconciled nor prepared.
	runner.incoming, runner.status.SyncConflict, runner.status.SyncBlocked = pending, "", ""
	started := runner.status.Scan.Started

	runner.syncRemote(t.Context(), fetched.Add(time.Hour))

	if !runner.status.Scan.Started.Equal(started) || !strings.Contains(runner.status.SyncConflict, "4096") {
		t.Fatal("rejected target was reconciled and prepared again", runner.status)
	}

	assertIncomingFile(t, filepath.Join(runner.roots[0], "a"), "initial")

	incomingGit(t, editor, "rm", "-r", "-q", "--", directory)
	target := incomingEdit(t, runner, editor, "a", "remote bytes")
	runner.syncRemote(t.Context(), fetched.Add(time.Hour+time.Hour))

	pending, err = runner.catalog.Incoming(t.Context())
	if err != nil || pending != nil || runner.status.InboundApplied != target || runner.status.SyncConflict != "" {
		t.Fatal("new remote commit not retried after a rejected one", pending, err, runner.status)
	}

	assertIncomingFile(t, filepath.Join(runner.roots[0], "a"), "remote bytes")
}
