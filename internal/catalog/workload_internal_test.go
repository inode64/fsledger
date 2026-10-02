package catalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/resource"
)

//nolint:gosec // Synthetic indices are nonnegative and bounded by the explicit workload size.
func workloadRecord(index int) integrity.Record {
	path := fmt.Sprintf("/data/dir-%06d/file-%08d", index/1000, index)
	digest := sha256.Sum256([]byte(path))

	return integrity.Record{
		Path:             []byte(path),
		Type:             fixtureRegular,
		Algorithm:        fixtureSHA256,
		Hash:             hex.EncodeToString(digest[:]),
		Size:             4096,
		Inode:            uint64(index + 1),
		UID:              1000,
		GID:              1000,
		Mode:             0o600,
		Mtime:            1700000000000000000,
		Ctime:            1700000000000000000,
		Atime:            1700000000000000000,
		Observed:         1700000000000000000,
		AttributesStatus: "available",
	}
}

// TestCatalogWorkload is opt-in; it measures this runtime adapter without reading physical source files.
func TestCatalogWorkload(t *testing.T) {
	t.Parallel()

	requested := os.Getenv("FSLEDGER_BENCH_RECORDS")
	if requested == "" {
		t.Skip("set FSLEDGER_BENCH_RECORDS for the runtime storage workload")
	}

	count, err := strconv.Atoi(requested)
	if err != nil || count < 1000 {
		t.Fatal("invalid workload size")
	}

	directory := filepath.Join(t.TempDir(), "catalog.pebble")
	store := testStoreAt(t, directory)
	store.Notifications.Use = nil
	started := time.Now()

	loadWorkload(t, store, count)

	loaded := time.Now()
	store.state.Complete = true

	err = store.InitBaseline(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	approved := time.Now()

	mutateWorkload(t, store, count/100, false)

	modified := time.Now()

	mutateWorkload(t, store, count/1000, true)

	deleted := time.Now()

	err = store.ClearOperation(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	verifyWorkload(t, store, count-count/1000)

	checked := time.Now()
	size := compactSize(t, store, directory)
	t.Logf(
		"WORKLOAD records=%d bytes=%d load=%s baseline=%s update_1pct=%s delete_0.1pct=%s verify=%s total=%s",
		count,
		size,
		loaded.Sub(started),
		approved.Sub(loaded),
		modified.Sub(approved),
		deleted.Sub(modified),
		checked.Sub(deleted),
		time.Since(started),
	)
}

func mutateWorkload(t *testing.T, store *Store, count int, deleted bool) {
	t.Helper()

	for start := 0; start < count; start += transactionBatchSize {
		identifier := changeID()

		records := make([]integrity.Record, 0, transactionBatchSize)
		for index := start; index < min(start+transactionBatchSize, count); index++ {
			record := workloadRecord(index)
			if deleted {
				record = integrity.Record{Path: record.Path}
			} else {
				record.Hash = strings.Repeat("f", 64)
			}

			records = append(records, record)
		}

		observeRecords(t, store, identifier, records...)

		operationErr1 := store.Accept(t.Context(), identifier)
		if operationErr1 != nil {
			t.Fatal(operationErr1)
		}
	}
}

func verifyWorkload(t *testing.T, store *Store, expected int) {
	t.Helper()

	count := 0

	err := store.iterate(t.Context(), currentPrefix, func(path, data []byte) error {
		record, decodeErr := unpack(data, path)
		if decodeErr != nil {
			return decodeErr
		}

		reference, _, readErr := readRecord(store.database, baselinePrefix(store.state.Baseline), path)
		if readErr != nil {
			return readErr
		}

		if len(integrity.Differences(reference, record, store.Policy.Compare)) != 0 {
			t.Fatal("approved state mismatch")
		}

		count++

		return nil
	})
	if err != nil || count != expected {
		t.Fatalf("expected %d records, got %d: %v", expected, count, err)
	}

	stats, err := store.Stats(t.Context())
	if err != nil || stats.Entries != int64(expected) || stats.Violations != 0 {
		t.Fatal("invalid workload counters", stats, err)
	}
}

//nolint:gosec // Private opt-in fixture; validated nonnegative file sizes and block counts fit uint64.
func compactSize(t *testing.T, store *Store, directory string) uint64 {
	t.Helper()

	err := store.database.Flush()
	if err != nil {
		t.Fatal(err)
	}

	err = store.database.Compact(t.Context(), []byte{0}, []byte{255}, true)
	if err != nil {
		t.Fatal(err)
	}

	err = store.Close()
	if err != nil {
		t.Fatal(err)
	}

	store.database = nil

	var size, allocated uint64

	err = filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if entry.IsDir() {
			return nil
		}

		info, statErr := os.Stat(path)
		if statErr != nil {
			return fault.Wrap("stat benchmark file", statErr)
		}

		if info.Size() < 0 {
			t.Fatal("negative file size")
		}

		size += uint64(info.Size())

		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Blocks < 0 {
			t.Fatal("missing Linux allocation")
		}

		const blockBytes = 512

		allocated += uint64(stat.Blocks) * blockBytes

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("CLOSED logical_bytes=%d allocated_bytes=%d", size, allocated)

	return size
}

// TestCatalogDataset replays exported reference/current records from a private benchmark fixture.
//
//nolint:paralleltest,tparallel // Measure repositories sequentially and aggregate disk usage deterministically.
func TestCatalogDataset(t *testing.T) {
	t.Parallel()

	root := os.Getenv("FSLEDGER_BENCH_DATASET")
	if root == "" {
		t.Skip("set FSLEDGER_BENCH_DATASET to reviewed JSON streams")
	}

	names, operationErr1 := filepath.Glob(filepath.Join(root, "*", "baseline.jsonl"))
	if operationErr1 != nil || len(names) == 0 {
		t.Fatal("missing dataset", operationErr1)
	}

	var total uint64

	for _, path := range names {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) { total += replayDataset(t, path) })
	}

	t.Logf("DATASET repositories=%d bytes=%d", len(names), total)
}

//nolint:gosec // Opt-in benchmark reads only the explicitly supplied private dataset.
func replayDataset(t *testing.T, path string) uint64 {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "catalog.pebble")
	store := testStoreAt(t, directory)
	store.Policy.Compare = []string{"hash", "inode", "target", "type"}
	store.Notifications.Use = nil

	input, operationErr1 := os.Open(path)
	if operationErr1 != nil {
		t.Fatal(operationErr1)
	}
	defer resource.Close(input)

	operationErr1 = store.ImportBaseline(t.Context(), input)
	if operationErr1 != nil {
		t.Fatal(operationErr1)
	}

	current, operationErr1 := os.Open(filepath.Join(filepath.Dir(path), "current.jsonl"))
	if operationErr1 != nil {
		t.Fatal(operationErr1)
	}
	defer resource.Close(current)

	decoder := json.NewDecoder(current)
	for {
		records, readErr := importRecords(decoder)
		if readErr != nil {
			t.Fatal(readErr)
		}

		if len(records) == 0 {
			break
		}

		observeRecords(t, store, changeID(), records...)
	}
	// Verify every field against both streams, not just the configured comparison policy.
	checkStream(t, store, path, baselinePrefix(store.state.Baseline))
	checkStream(t, store, filepath.Join(filepath.Dir(path), "current.jsonl"), currentPrefix)

	stats, operationErr1 := store.Stats(t.Context())
	if operationErr1 != nil {
		t.Fatal(operationErr1)
	}

	size := compactSize(t, store, directory)
	t.Logf("DATASET entries=%d pending=%d bytes=%d", stats.Entries, stats.Violations, size)

	return size
}

//nolint:gosec // Opt-in benchmark reads the explicitly supplied private dataset.
func checkStream(t *testing.T, store *Store, path, prefix string) {
	t.Helper()

	input, operationErr1 := os.Open(path)
	if operationErr1 != nil {
		t.Fatal(operationErr1)
	}
	defer resource.Close(input)

	decoder := json.NewDecoder(input)
	for {
		records, readErr := importRecords(decoder)
		if readErr != nil {
			t.Fatal(readErr)
		}

		if len(records) == 0 {
			break
		}

		for _, record := range records {
			expected, encodeErr := packRecord(record)
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}

			actual, getErr := get(store.database, key(prefix, record.Path))
			if getErr != nil || !bytes.Equal(actual, expected) {
				t.Fatalf("record mismatch %q: %v", record.Path, getErr)
			}
		}
	}
}

func loadWorkload(t *testing.T, store *Store, count int) {
	t.Helper()

	for start := 0; start < count; start += transactionBatchSize {
		records := make([]integrity.Record, 0, transactionBatchSize)
		for index := start; index < min(start+transactionBatchSize, count); index++ {
			records = append(records, workloadRecord(index))
		}

		observeRecords(t, store, "initial", records...)
	}
}
