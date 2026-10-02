//nolint:testpackage // The scale gate measures the production catalog with its existing internal fixtures.
package catalog

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/integrity"
)

// TestScalePollingInventory uses the production scanner and catalog. It needs
// enough private storage/inodes for the requested tree; it changes no sysctls.
func TestScalePollingInventory(t *testing.T) {
	t.Parallel()

	if os.Getenv("FSLEDGER_SCALE_SCAN") != "1" {
		t.Skip("opt in with FSLEDGER_SCALE_SCAN=1; creates 2M files and 337520 directories by default")
	}

	files := scaleCount(t, "FSLEDGER_SCALE_FILES", 2000000)

	directories := scaleCount(t, "FSLEDGER_SCALE_DIRS", 337520)
	if directories > files {
		t.Fatal("scale directories must not exceed files")
	}

	source := t.TempDir()
	makeScaleTree(t, source, files, directories)
	store := testStore(t)
	now := summaryFixture(t, store, 0)
	scanner := integrity.NewScanner(4, 1)

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	for _, phase := range []string{"initial", "unchanged", "deployment", "rehash"} {
		if phase == "deployment" {
			mutateScaleTree(t, source, directories)
		}

		started := time.Now()

		result, scanErr := store.Reconcile(t.Context(), scanner, []string{source}, matcher,
			phase == "initial" || phase == "rehash", phase, phase)
		if scanErr != nil {
			t.Fatal(scanErr)
		}

		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		t.Logf("phase=%s elapsed=%s observed=%d hashed=%d changed=%d heap_inuse=%d go_sys=%d",
			phase, time.Since(started), result.Observed, result.Hashed, result.Changed, memory.HeapInuse, memory.Sys)

		if result.Observed != int64(files+directories+1) || (phase == "unchanged" && result.Changed != 0) {
			t.Fatalf("incorrect full repository scan: %+v", result)
		}
	}

	*now = now.Add(time.Minute)
	started := time.Now()
	draft := nextReport(t, store)
	t.Logf("summary elapsed=%s observations=%d sample=%d", time.Since(started),
		draft.Message.Count, len(draft.Message.Report.Items))

	if draft.Message.Report.More || len(draft.Message.Report.Items) > reportSampleSize {
		t.Fatal("summary is not bounded")
	}
}

func scaleCount(t *testing.T, name string, fallback int) int {
	t.Helper()

	value := os.Getenv(name)
	if value == "" {
		return fallback
	}

	count, err := strconv.Atoi(value)
	if err != nil || count < 1 {
		t.Fatalf("invalid %s=%q", name, value)
	}

	return count
}

func makeScaleTree(t *testing.T, source string, files, directories int) {
	t.Helper()

	started := time.Now()

	for directory := range directories {
		path := filepath.Join(source, strconv.Itoa(directory))

		err := os.Mkdir(path, 0o700)
		if err != nil {
			t.Fatal(err)
		}

		for index := directory; index < files; index += directories {
			err = os.WriteFile(filepath.Join(path, strconv.Itoa(index)), []byte("before\n"), 0o600)
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Logf("fixture files=%d directories=%d creation=%s", files, directories, time.Since(started))
}

func mutateScaleTree(t *testing.T, source string, directories int) {
	t.Helper()

	for index := range min(directories, 5000) {
		path := filepath.Join(source, strconv.Itoa(index), strconv.Itoa(index))

		err := os.WriteFile(path, []byte("after\n"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}
}
