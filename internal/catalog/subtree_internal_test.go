package catalog

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/integrity"
)

func subtreeFixture(t *testing.T) (*Store, *integrity.Scanner, *exclude.Matcher, string) {
	t.Helper()
	store := testStore(t)

	root := t.TempDir()
	for _, name := range []string{"a/gone", "a/kept", "ab/sibling", "b/file", "direct-before", "direct-after"} {
		path := filepath.Join(root, name)

		err := os.MkdirAll(filepath.Dir(path), 0o700)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(path, []byte("original"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	scanner := integrity.NewScanner(1, 2)

	_, err = store.Reconcile(t.Context(), scanner, []string{root}, matcher, true, "initial", "")
	if err != nil {
		t.Fatal(err)
	}

	err = store.InitBaseline(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	return store, scanner, matcher, root
}

//nolint:cyclop,funlen,gocognit,gocyclo // Ordered integration assertions cover scope, approval and mixed events.
func TestDirectoryObservationPreservesScopeAndOperation(t *testing.T) {
	t.Parallel()
	store, scanner, matcher, root := subtreeFixture(t)
	fullScan := store.state.Stats.LastFullScan
	sibling := filepath.Join(root, "ab", "sibling")

	before, _, err := store.Current(t.Context(), []byte(sibling))
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"ab/sibling", "direct-before", "direct-after", "b/file", "a/kept"} {
		err = os.WriteFile(filepath.Join(root, name), []byte("changed"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	err = os.Remove(filepath.Join(root, "a", "gone"))
	if err != nil {
		t.Fatal(err)
	}

	paths := []string{
		filepath.Join(root, "direct-before"),
		filepath.Join(root, "a", "kept"),
		filepath.Join(root, "a"),
		filepath.Join(root, "b"),
		filepath.Join(root, "direct-after"),
	}

	result, err := store.Observe(t.Context(), scanner, paths, []string{root}, matcher, "actor", "directory-group")
	if err != nil || result.ID != "directory-group" || result.Changed != 5 {
		t.Fatal("mixed group lost updates or scanned unrelated paths", result, err)
	}

	after, exists, err := store.Current(t.Context(), []byte(sibling))
	if err != nil || !exists || after.Hash != before.Hash || after.Observed != before.Observed {
		t.Fatal("sibling was observed or pruned", err)
	}

	_, exists, err = store.Current(t.Context(), []byte(filepath.Join(root, "a", "gone")))

	if err != nil || exists {
		t.Fatal("missing child not pruned", err)
	}

	if store.state.Complete || store.state.Stats.LastFullScan != fullScan {
		t.Fatal("subtree certified a complete inventory")
	}

	err = store.InitBaseline(t.Context())
	if err == nil {
		t.Fatal("baseline initialization accepted partial scan")
	}

	err = store.Accept(t.Context(), result.ID)
	if err != nil {
		t.Fatal("change ID cannot be accepted", err)
	}

	if len(pendingIDs(t, store)) != 0 {
		t.Fatal("group approval lost a subtree or direct file")
	}

	_, err = store.Reconcile(t.Context(), scanner, []string{root}, matcher, false, "periodic", "")
	if err != nil {
		t.Fatal(err)
	}

	if len(pendingIDs(t, store)) != 1 || pendingIDs(t, store)[sibling] == "" {
		t.Fatal("periodic scan failed to detect unrelated modification")
	}
}

func TestIncompleteSubtreeDoesNotPrune(t *testing.T) {
	t.Parallel()

	store, scanner, matcher, root := subtreeFixture(t)

	err := os.Remove(filepath.Join(root, "a", "gone"))
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(root, "a", "kept"), []byte("changed"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	generation := store.state.Generation

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	store.copiedHashes = func(integrity.Record, string) (string, int64, bool) {
		cancel()

		return "", 0, false
	}

	_, err = store.Observe(
		ctx,
		scanner,
		[]string{filepath.Join(root, "a")},
		[]string{root},
		matcher,
		"actor",
		"cancelled",
	)
	if err == nil {
		t.Fatal("cancelled scan succeeded")
	}

	if store.state.Generation <= generation || store.state.Complete {
		t.Fatal("cancellation did not interrupt an active partial scan")
	}

	_, exists, err := store.Current(t.Context(), []byte(filepath.Join(root, "a", "gone")))

	if err != nil || !exists {
		t.Fatal("cancelled scan pruned unseen child", err)
	}
}

func TestSubtreePrefixEndingInFF(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	root := t.TempDir()
	directory := filepath.Join(root, "dir-"+string([]byte{0xff}))
	child := filepath.Join(directory, "child")

	err := os.Mkdir(directory, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(child, []byte("content"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	scanner := integrity.NewScanner(1, 2)

	_, err = store.Reconcile(t.Context(), scanner, []string{root}, matcher, true, "initial", "")
	if err != nil {
		t.Fatal(err)
	}

	err = os.Remove(child)
	if err != nil {
		t.Fatal(err)
	}

	result, err := store.Observe(
		t.Context(), scanner, []string{directory}, []string{root}, matcher, "actor", "ff-directory",
	)
	if err != nil || result.Changed != 1 {
		t.Fatal("0xff-terminated subtree prefix was not scanned", result, err)
	}
}

//nolint:funlen,paralleltest // Keep this opt-in filesystem workload and process-wide measurements together.
func TestDirectoryObservationWorkload(t *testing.T) {
	if os.Getenv("FSLEDGER_BENCH_SUBTREE") == "" {
		t.Skip("set FSLEDGER_BENCH_SUBTREE for filesystem workload")
	}

	store := testStore(t)
	store.Notifications.Use = nil

	root := t.TempDir()
	for _, dir := range []string{"affected", "unrelated"} {
		err := os.Mkdir(filepath.Join(root, dir), 0o700)
		if err != nil {
			t.Fatal(err)
		}
	}

	for index := range 3020 {
		dir := "unrelated"
		if index < 20 {
			dir = "affected"
		}

		err := os.WriteFile(filepath.Join(root, dir, strconv.Itoa(index)), []byte("original"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	scanner := integrity.NewScanner(1, 2)

	_, err = store.Reconcile(t.Context(), scanner, []string{root}, matcher, true, "initial", "")
	if err != nil {
		t.Fatal(err)
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	wal := store.database.Metrics().WAL.BytesWritten
	started := time.Now()

	for range 10 {
		_, err = store.Observe(
			t.Context(),
			scanner,
			[]string{filepath.Join(root, "affected")},
			[]string{root},
			matcher,
			"actor",
			"directory-event",
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	elapsed := time.Since(started)

	runtime.ReadMemStats(&after)
	t.Logf(
		"SUBTREE iterations=10 files=3020 affected=20 elapsed=%s allocated=%d allocations=%d WAL_bytes=%d",
		elapsed,
		after.TotalAlloc-before.TotalAlloc,
		after.Mallocs-before.Mallocs,
		store.database.Metrics().WAL.BytesWritten-wal,
	)
}

func TestReportClosedDuringSubtreeScanMarksCoverageGap(t *testing.T) {
	t.Parallel()
	store, scanner, matcher, root := subtreeFixture(t)
	now := configureReportClock(t, store)
	settings := reportSettings()

	settings.SendEmpty = true

	err := store.ConfigureReports(t.Context(), settings, false)
	if err != nil {
		t.Fatal(err)
	}

	*now = now.Add(time.Minute)

	err = os.WriteFile(filepath.Join(root, "a", "kept"), []byte("changed"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	var (
		draft     *ReportDraft
		reportErr error
	)

	store.BindCopiedHashes(func(integrity.Record, string) (string, int64, bool) {
		draft, reportErr = store.NextReport(t.Context())

		return "", 0, false
	})

	_, err = store.Observe(
		t.Context(),
		scanner,
		[]string{filepath.Join(root, "a")},
		[]string{root},
		matcher,
		"actor",
		"subtree",
	)
	if err != nil || reportErr != nil {
		t.Fatal(err, reportErr)
	}

	if draft == nil || !draft.Message.Report.CoverageGap {
		t.Fatal("subtree scan was reported as complete")
	}

	if store.scanActive {
		t.Fatal("finished subtree left scan active")
	}
}
