// Package catalog persists current observations and pending deliveries.
package catalog

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/sstable"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/filter"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/mountinfo"
	"github.com/inode64/fsledger/internal/reportclock"
	"github.com/inode64/fsledger/internal/resource"
)

const (
	catalogFormat           = "fsledger-pebble-protobuf-snappy-1"
	currentPrefix           = "e/"
	seenPrefix              = "s/"
	hashPrefix              = "h/"
	intentPrefix            = "i/"
	outboxPrefix            = "o/"
	duePrefix               = "d/"
	notificationStagePrefix = "n/"
	stateKey                = "meta/state"
	formatKey               = "meta/format"
	sharedCacheBytes        = 32 << 20
	memtableBytes           = 512 << 10
	// Pebble charges a store's memtables to its cache: the mutable one and the arena kept for recycling.
	storeMemtableBytes = 2 * memtableBytes
	maximumByte        = 0xff
)

// A process-wide cache bounds block memory across independently locked repositories.
//
// Pebble reserves every store's memtables out of the cache it is given. With a fixed capacity a few dozen
// stores reserve all of it, no block stays cached and each lookup reloads and decompresses its blocks.
// The capacity therefore adds a memtable allowance per expected store, which leaves sharedCacheBytes
// for blocks however many repositories the daemon serves.
type sharedCache struct {
	cache    *pebble.Cache
	users    int
	expected int
	mutex    sync.Mutex
}

//nolint:gochecknoglobals // Shared ownership is protected by the mutex and released after the last store closes.
var caches sharedCache

// ExpectStores declares how many catalogs this process keeps open together. It sizes the next shared
// cache, so the daemon calls it before opening its repositories.
func ExpectStores(count int) { caches.expect(count) }

func (shared *sharedCache) expect(count int) {
	shared.mutex.Lock()
	defer shared.mutex.Unlock()

	shared.expected = count
}

func (shared *sharedCache) acquire() *pebble.Cache {
	shared.mutex.Lock()
	defer shared.mutex.Unlock()

	if shared.cache == nil {
		shared.cache = pebble.NewCache(sharedCacheBytes + int64(max(shared.expected, 1))*storeMemtableBytes)
	}

	shared.users++

	return shared.cache
}

func (shared *sharedCache) release() {
	shared.mutex.Lock()
	defer shared.mutex.Unlock()

	shared.users--
	if shared.users == 0 {
		shared.cache.Unref()
		shared.cache = nil
	}
}

// Store serializes writes per repository; scans share a separate operation lock.
// Its integrity settings are fixed at Open; a change requires closing and reopening the store.
type Store struct {
	reportCalendar *reportclock.Schedule
	now            func() time.Time
	ignore         *filter.Matcher
	copiedHashes   integrity.HashSource
	database       *pebble.DB
	versions       map[string]string
	windows        map[string]notificationWindow
	reportHead     string
	Repository     string
	Host           string
	policy         config.Integrity
	reports        config.Reports
	Notifications  config.Notifications
	state          diskState
	mutex          sync.Mutex
	operation      sync.Mutex
	scanActive     bool
}

// Stats describes live state, never cumulative change history. ExhaustedNotifications counts the pending
// notifications that stopped retrying.
type Stats struct {
	DeferredPaths          int64 `json:"deferred_paths"`
	DeferredSince          int64 `json:"deferred_since"`
	Entries                int64 `json:"entries"`
	NotificationOverflow   int64 `json:"notification_overflow"`
	PendingNotifications   int64 `json:"pending_notifications"`
	ExhaustedNotifications int64 `json:"exhausted_notifications,omitzero"`
	LastFullScan           int64 `json:"last_full_scan"`
}

type diskState struct {
	// Mounts are the mount points recorded for the sources; see RememberMounts.
	Mounts                 []mountinfo.Record `json:"mounts,omitempty"`
	Reports                reportState        `json:"reports"`
	Stats                  Stats              `json:"stats"`
	Generation             uint64             `json:"generation"`
	NextDelivery           int64              `json:"next_delivery"`
	InitialSnapshotPending bool               `json:"initial_snapshot_pending,omitempty"`
}

// Open opens the current directory format.
func Open(
	ctx context.Context,
	path, repository, host string,
	policy config.Integrity,
	notifications config.Notifications,
) (*Store, error) {
	ignore, err := filter.Compile(notifications.Ignore)
	if err != nil {
		return nil, err
	}

	operationErr1 := ctx.Err()
	if operationErr1 != nil {
		return nil, fault.Wrap("open catalog", operationErr1)
	}

	operationErr2 := checkDirectory(path)
	if operationErr2 != nil {
		return nil, operationErr2
	}

	options := &pebble.Options{}
	options.Logger = engineLogger{repository: repository}
	options.EnsureDefaults()
	options.Cache = caches.acquire()
	options.MemTableSize = memtableBytes

	options.CompactionConcurrencyRange = func() (int, int) { return 1, 1 }
	for index := range options.Levels {
		options.Levels[index].Compression = func() *sstable.CompressionProfile { return sstable.SnappyCompression }
	}

	database, err := pebble.Open(path, options)
	if err != nil {
		caches.release()

		return nil, fault.Wrap("open Pebble catalog", err)
	}

	store := &Store{
		now:           time.Now,
		ignore:        ignore,
		windows:       make(map[string]notificationWindow),
		database:      database,
		Repository:    repository,
		Host:          host,
		policy:        policy,
		Notifications: notifications,
	}

	err = store.initialize(ctx)
	if err != nil {
		resource.Close(store)

		return nil, err
	}

	return store, nil
}

func checkDirectory(path string) error {
	err := config.CheckDirectory("catalog", path)
	if err != nil {
		return err
	}
	// #nosec G703 -- CheckDirectory rejects a catalog path through a symlink or a non-directory.
	return fault.Wrap("create catalog directory", os.MkdirAll(path, 0o700))
}

// BindDestinations freezes routing identity when a delivery is enqueued.
func (store *Store) BindDestinations(notifiers map[string]config.Notifier) {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	store.versions = make(map[string]string, len(notifiers))
	for name, notifier := range notifiers {
		store.versions[name] = config.NotifierVersion(notifier)
	}
}

// Close releases the database and its shared cache reference after users have stopped.
func (store *Store) Close() error {
	err := store.database.Close()

	caches.release()

	return fault.Wrap("close catalog", err)
}

// Stats returns persisted counters without scanning the inventory.
func (store *Store) Stats(ctx context.Context) (Stats, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	return store.state.Stats, fault.Wrap("catalog status", ctx.Err())
}

// RememberMounts records the mount points that hold or lie inside the sources, kept across restarts: one no
// longer mounted keeps the entries below it, which its filesystem took away rather than deleted.
func (store *Store) RememberMounts(ctx context.Context, mounts []mountinfo.Record) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	if slices.Equal(store.state.Mounts, mounts) {
		return nil
	}

	err := ctx.Err()
	if err != nil {
		return fault.Wrap("record mount points", err)
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	transaction.state.Mounts = slices.Clone(mounts)

	return transaction.commit()
}

// Mounts returns the recorded mount points, those of the previous process after a restart.
func (store *Store) Mounts() []mountinfo.Record {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	return slices.Clone(store.state.Mounts)
}

// BindCopiedHashes is called by the owner before inventory operations begin.
func (store *Store) BindCopiedHashes(source integrity.HashSource) { store.copiedHashes = source }

// kept lists the paths set aside while their recorded filesystem is not mounted: they are neither walked
// nor deleted.
func (store *Store) kept(roots []string) []string { return mountinfo.Kept(store.Mounts(), roots) }

func (store *Store) initialize(ctx context.Context) error {
	format, err := get(store.database, []byte(formatKey))
	if err != nil {
		return err
	}

	err = store.loadState(format)
	if err != nil {
		return err
	}

	if format != nil {
		return store.dropRetiredKeys(ctx)
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	transaction.set([]byte(formatKey), []byte(catalogFormat))

	return transaction.commit()
}

// dropRetiredKeys deletes the baseline (b/), pending approval (p/), token (t/) and approval (a/) keys that
// catalogs written before their removal still hold. Remove it once every deployed catalog has been opened.
func (store *Store) dropRetiredKeys(ctx context.Context) error {
	transaction := store.begin()
	defer resource.Close(transaction.batch)

	found := false

	for _, prefix := range []string{"b/", "p/", "t/", "a/"} {
		err := store.iterate(ctx, prefix, func(_, _ []byte) error {
			found = true

			transaction.clear(prefix)

			return errStop
		})
		if withoutStop(err) != nil {
			return withoutStop(err)
		}
	}

	if !found {
		return nil
	}

	return transaction.commit()
}

func changeID() string                      { return rand.Text() }
func key(prefix string, path []byte) []byte { return append([]byte(prefix), path...) }
func upper(prefix string) []byte {
	result := append([]byte(nil), prefix...)
	for index := len(result) - 1; index >= 0; index-- {
		if result[index] == maximumByte {
			continue
		}

		result[index]++

		return result[:index+1]
	}

	return nil
}

func encode(value any) ([]byte, error) {
	data, err := json.Marshal(value)

	return data, fault.Wrap("encode catalog value", err)
}

type reader interface {
	Get(lookup []byte) ([]byte, io.Closer, error)
}

func get(source reader, lookup []byte) ([]byte, error) {
	data, closer, err := source.Get(lookup)
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, nil
	}

	if err != nil {
		return nil, fault.Wrap("read catalog value", err)
	}

	copied := append([]byte{}, data...)

	return copied, fault.Wrap("release catalog value", closer.Close())
}

func readRecord(source reader, prefix string, path []byte) (integrity.Record, []byte, error) {
	data, err := get(source, key(prefix, path))
	if err != nil || data == nil {
		return integrity.Record{}, data, err
	}

	record, err := unpack(data, path)

	return record, data, err
}

type transaction struct {
	notifications map[notificationRoute]notificationBatch
	err           error
	store         *Store
	batch         *pebble.Batch
	state         diskState
}

func (store *Store) begin() *transaction {
	return &transaction{
		notifications: nil,
		store:         store,
		batch:         store.database.NewIndexedBatch(),
		state:         store.state,
		err:           nil,
	}
}

func (transaction *transaction) set(lookup, value []byte) {
	if transaction.err == nil {
		transaction.err = transaction.batch.Set(lookup, value, nil)
	}
}

func (transaction *transaction) remove(lookup []byte) {
	if transaction.err == nil {
		transaction.err = transaction.batch.Delete(lookup, nil)
	}
}

func (transaction *transaction) clear(prefix string) {
	if transaction.err == nil {
		transaction.err = transaction.batch.DeleteRange([]byte(prefix), upper(prefix), nil)
	}
}

func (transaction *transaction) put(lookup []byte, value any) {
	data, err := encode(value)
	if err != nil {
		transaction.err = err

		return
	}

	transaction.set(lookup, data)
}

func (transaction *transaction) commit() error {
	transaction.flushNotifications()
	transaction.put([]byte(stateKey), transaction.state)

	if transaction.err != nil {
		return fault.Wrap("prepare catalog transaction", transaction.err)
	}

	err := transaction.batch.Commit(pebble.Sync)
	if err != nil {
		return fault.Wrap("commit catalog transaction", err)
	}

	transaction.store.state = transaction.state

	return nil
}

func (store *Store) iterate(ctx context.Context, prefix string, visit func([]byte, []byte) error) error {
	options := &pebble.IterOptions{}
	options.LowerBound, options.UpperBound = []byte(prefix), upper(prefix)

	return store.iterateRange(ctx, prefix, options, visit)
}

func (store *Store) iterateRange(
	ctx context.Context, prefix string, options *pebble.IterOptions, visit func([]byte, []byte) error,
) error {
	iterator, err := store.database.NewIter(options)
	if err != nil {
		return fault.Wrap("iterate catalog", err)
	}
	defer resource.Close(iterator)

	for iterator.First(); iterator.Valid(); iterator.Next() {
		err = ctx.Err()
		if err != nil {
			return fault.Wrap("iterate catalog", err)
		}

		err = visit(iterator.Key()[len(prefix):], iterator.Value())
		if err != nil {
			return err
		}
	}

	return fault.Wrap("iterate catalog", iterator.Error())
}

func (store *Store) loadState(format []byte) error {
	if format == nil {
		return store.checkEmpty()
	}

	if string(format) != catalogFormat {
		return fault.New("unsupported catalog format")
	}

	data, err := get(store.database, []byte(stateKey))
	if err != nil {
		return err
	}

	err = json.Unmarshal(data, &store.state)

	return fault.Wrap("decode catalog state", err)
}

func (store *Store) checkEmpty() error {
	iterator, err := store.database.NewIter(nil)
	if err != nil {
		return fault.Wrap("inspect catalog", err)
	}
	defer resource.Close(iterator)

	if iterator.First() {
		return fault.New("unversioned nonempty catalog")
	}

	return fault.Wrap("inspect catalog", iterator.Error())
}
