// Package catalog persists current observations, approved references and pending deliveries.
package catalog

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/sstable"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/filter"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/reportclock"
	"github.com/inode64/fsledger/internal/resource"
)

const (
	catalogFormat    = "fsledger-pebble-protobuf-snappy-1"
	currentPrefix    = "e/"
	seenPrefix       = "s/"
	pendingPrefix    = "p/"
	tokenPrefix      = "t/"
	approvalPrefix   = "a/"
	hashPrefix       = "h/"
	intentPrefix     = "i/"
	outboxPrefix     = "o/"
	duePrefix        = "d/"
	stateKey         = "meta/state"
	formatKey        = "meta/format"
	sharedCacheBytes = 32 << 20
	memtableBytes    = 512 << 10
	maximumByte      = 0xff
)

// A process-wide cache bounds block memory across independently locked repositories.
//
//nolint:gochecknoglobals // Shared ownership is protected by the mutex and released after the last store closes.
var caches struct {
	cache *pebble.Cache
	users int
	mutex sync.Mutex
}

func acquireCache() *pebble.Cache {
	caches.mutex.Lock()
	defer caches.mutex.Unlock()

	if caches.cache == nil {
		caches.cache = pebble.NewCache(sharedCacheBytes)
	}

	caches.users++

	return caches.cache
}

func releaseCache() {
	caches.mutex.Lock()
	defer caches.mutex.Unlock()

	caches.users--
	if caches.users == 0 {
		caches.cache.Unref()
		caches.cache = nil
	}
}

// Store serializes writes per repository; scans and approvals share a separate operation lock.
type Store struct {
	reportCalendar    *reportclock.Schedule
	now               func() time.Time
	ignore            *filter.Matcher
	copiedHashes      integrity.HashSource
	database          *pebble.DB
	versions          map[string]string
	windows           map[string]notificationWindow
	policyFields      []string
	policyAlgorithm   string
	policyDigest      string
	reportHead        string
	Repository        string
	Host              string
	Policy            config.Integrity
	reports           config.Reports
	Notifications     config.Notifications
	state             diskState
	mutex             sync.Mutex
	operation         sync.Mutex
	scanActive        bool
	replacingBaseline bool
}

// Stats describes live state, never cumulative change history.
type Stats struct {
	DeferredPaths        int64 `json:"deferred_paths"`
	DeferredSince        int64 `json:"deferred_since"`
	Entries              int64 `json:"entries"`
	Violations           int64 `json:"violations"`
	NotificationOverflow int64 `json:"notification_overflow"`
	PendingNotifications int64 `json:"pending_notifications"`
	BaselineReady        bool  `json:"baseline_ready"`
	LastFullScan         int64 `json:"last_full_scan"`
}

type diskState struct {
	Policy       string      `json:"policy"`
	Reports      reportState `json:"reports"`
	Stats        Stats       `json:"stats"`
	Baseline     int         `json:"baseline"`
	Generation   uint64      `json:"generation"`
	NextDelivery int64       `json:"next_delivery"`
	Complete     bool        `json:"complete"`
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
	options.Cache = acquireCache()
	options.MemTableSize = memtableBytes

	options.CompactionConcurrencyRange = func() (int, int) { return 1, 1 }
	for index := range options.Levels {
		options.Levels[index].Compression = func() *sstable.CompressionProfile { return sstable.SnappyCompression }
	}

	database, err := pebble.Open(path, options)
	if err != nil {
		releaseCache()

		return nil, fault.Wrap("open Pebble catalog", err)
	}

	store := &Store{
		now:           time.Now,
		ignore:        ignore,
		windows:       make(map[string]notificationWindow),
		database:      database,
		Repository:    repository,
		Host:          host,
		Policy:        policy,
		Notifications: notifications,
	}

	err = store.initialize()
	if err != nil {
		resource.Close(store)

		return nil, err
	}

	return store, nil
}

func checkDirectory(path string) error {
	err := inspectDirectory(path)
	if err != nil {
		return err
	}
	// #nosec G703 -- inspectDirectory rejects every existing symlink or non-directory component.
	return fault.Wrap("create catalog directory", os.MkdirAll(path, 0o700))
}

func inspectDirectory(path string) error {
	// Check every existing component before MkdirAll; do not follow catalog symlinks.
	for part := filepath.Clean(path); ; part = filepath.Dir(part) {
		// #nosec G703 -- Lstat inspects configured ancestors without following links or reading file contents.
		info, err := os.Lstat(part)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fault.Wrap("inspect catalog directory", err)
		}

		if err == nil && !info.IsDir() {
			return fault.New("catalog and its parents must be directories, not symlinks or legacy files")
		}

		if filepath.Dir(part) == part {
			break
		}
	}

	return nil
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

	releaseCache()

	return fault.Wrap("close catalog", err)
}

// Stats returns persisted counters without scanning the inventory.
func (store *Store) Stats(ctx context.Context) (Stats, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	return store.state.Stats, fault.Wrap("catalog status", ctx.Err())
}

// BindCopiedHashes is called by the owner before inventory operations begin.
func (store *Store) BindCopiedHashes(source integrity.HashSource) { store.copiedHashes = source }

func (store *Store) initialize() error {
	format, err := get(store.database, []byte(formatKey))
	if err != nil {
		return err
	}

	err = store.loadState(format)
	if err != nil {
		return err
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	transaction.set([]byte(formatKey), []byte(catalogFormat))
	// A crash before publication may leave an unfinished reference in the inactive slot.
	transaction.clear(baselinePrefix(1 - store.state.Baseline))

	return transaction.commit()
}

func (store *Store) policyID() string {
	if store.policyDigest == "" || store.policyAlgorithm != store.Policy.Hash.Algorithm ||
		!slices.Equal(store.policyFields, store.Policy.Compare) {
		store.policyFields = slices.Clone(store.Policy.Compare)
		store.policyAlgorithm = store.Policy.Hash.Algorithm
		store.policyDigest = integrity.PolicyID(store.policyFields, store.policyAlgorithm)
	}

	return store.policyDigest
}
func changeID() string { return rand.Text() }
func baselinePrefix(slot int) string {
	if slot == 0 {
		return "b/0/"
	}

	return "b/1/"
}
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
	notifications map[string]notificationBatch
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
	if err != nil {
		return fault.Wrap("decode catalog state", err)
	}

	if store.state.Baseline < 0 || store.state.Baseline > 1 {
		return fault.New("invalid active reference slot")
	}

	return nil
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
