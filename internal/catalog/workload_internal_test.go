package catalog

import (
	"crypto/sha256"
	"encoding/hex"
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
		"WORKLOAD records=%d bytes=%d load=%s update_1pct=%s delete_0.1pct=%s verify=%s total=%s",
		count,
		size,
		loaded.Sub(started),
		modified.Sub(loaded),
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
	}
}

func verifyWorkload(t *testing.T, store *Store, expected int) {
	t.Helper()

	count := 0

	err := store.iterate(t.Context(), currentPrefix, func(path, data []byte) error {
		_, decodeErr := unpack(data, path)
		if decodeErr != nil {
			return decodeErr
		}

		count++

		return nil
	})
	if err != nil || count != expected {
		t.Fatalf("expected %d records, got %d: %v", expected, count, err)
	}

	stats, err := store.Stats(t.Context())
	if err != nil || stats.Entries != int64(expected) {
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
