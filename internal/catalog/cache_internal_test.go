package catalog

import "testing"

// The daemon announces its repositories so that their memtable reservations leave the block budget
// intact; a fixed capacity is exhausted by a few dozen open stores.
func TestSharedCacheAddsMemtableAllowancePerExpectedStore(t *testing.T) {
	t.Parallel()

	const stores = 60

	var shared sharedCache

	shared.expect(stores)

	cache := shared.acquire()
	defer shared.release()

	if got, want := cache.MaxSize(), int64(sharedCacheBytes+stores*storeMemtableBytes); got != want {
		t.Fatalf("shared cache holds %d bytes; want %d", got, want)
	}
}

// Pebble's reservation for a settled store must fit the allowance, or every open store eats into the
// budget meant for blocks.
func TestStoreMemtablesFitTheirCacheAllowance(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	loadWorkload(t, store, 24*transactionBatchSize)

	err := store.database.Flush()
	if err != nil {
		t.Fatal(err)
	}

	metrics := store.database.Metrics()
	if reserved := metrics.MemTable.Size + metrics.MemTable.ZombieSize; reserved > storeMemtableBytes {
		t.Fatalf("memtables reserve %d bytes of the shared cache; allowance is %d", reserved, storeMemtableBytes)
	}

	if metrics.MemTable.Size < memtableBytes {
		t.Fatalf("workload never filled a memtable: %d bytes", metrics.MemTable.Size)
	}
}
