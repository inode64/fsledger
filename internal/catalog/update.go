package catalog

import (
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/cockroachdb/pebble/v2"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/pathutil"
	"github.com/inode64/fsledger/internal/resource"
)

const (
	transactionBatchSize = 256
	changeAdded          = "added"
	changeDeleted        = "deleted"
	changeModified       = "modified"
)

// Result identifies one observation operation, not an archived version.
type Result struct {
	ID         string `json:"id"`
	Observed   int64  `json:"observed"`
	Hashed     int64  `json:"hashed"`
	Changed    int    `json:"changed"`
	Violations int    `json:"violations"`
}
type mutation struct {
	previous    *storedRecord
	record      integrity.Record
	excluded    bool
	deleted     bool
	refreshHash bool
}

// storedRecord is valid only within the serialized inventory operation that read it.
type storedRecord struct {
	data   []byte
	record integrity.Record
}

func commitBatch(batch *pebble.Batch) error {
	return fault.Wrap("commit reference batch", batch.Commit(pebble.Sync))
}

// Reconcile streams the inventory and prunes missing files only after a successful scan.
func (store *Store) Reconcile(
	ctx context.Context,
	scanner *integrity.Scanner,
	roots []string,
	matcher *exclude.Matcher,
	full bool, actor, identifier string,
) (Result, error) {
	store.operation.Lock()
	defer store.operation.Unlock()

	return store.reconcile(ctx, scanner, roots, matcher, full, actor, identifier)
}

func (store *Store) beginOperation(ctx context.Context, scan bool) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	err := ctx.Err()
	if err != nil {
		return fault.Wrap("begin inventory", err)
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	transaction.clear(hashPrefix)

	if scan {
		transaction.state.Generation++
	}

	transaction.state.Complete = false

	return transaction.commit()
}

func (store *Store) reconcile(
	ctx context.Context,
	scanner *integrity.Scanner,
	roots []string,
	matcher *exclude.Matcher,
	full bool, actor, identifier string,
	paths ...string,
) (Result, error) {
	store.setScanActive(true)
	defer store.setScanActive(false)

	result := observationResult(identifier)

	operationErr1 := store.beginOperation(ctx, true)
	if operationErr1 != nil {
		return result, operationErr1
	}

	generation := store.generation()
	batch := store.newBatcher(generation, &result, actor)

	algorithm := store.Policy.Hash.Algorithm
	content := full && slices.Contains(store.Policy.Compare, fieldHash)
	err := scanner.ScanPaths(ctx, roots, paths, matcher, algorithm, content, func(record integrity.Record) error {
		update, err := store.scanMutation(ctx, scanner, record, full)
		if err != nil {
			return err
		}

		result.Observed++
		// Full scans also verify digests supplied by this operation's mirror copy,
		// whose hash timestamp legitimately precedes the catalog observation.
		if update.record.Hash != "" && (full || update.record.HashedAt >= update.record.Observed) {
			result.Hashed++
		}

		return batch.add(ctx, update)
	},
		store.copiedHashes,
	)
	// Keep successfully observed paths even when another path is temporarily unstable.
	flushErr := batch.flush(ctx)

	unstable, fatal := integrity.SplitUnstable(err)
	if fatal != nil || flushErr != nil {
		return result, errors.Join(err, flushErr)
	}

	result, finishErr := store.finishScan(ctx, generation, result, actor, full, paths, matcher, unstable)

	return result, errors.Join(err, finishErr)
}

func (store *Store) scanMutation(
	ctx context.Context, scanner *integrity.Scanner, record integrity.Record, full bool,
) (mutation, error) {
	prepared, previous, err := store.prepare(ctx, scanner, record, full)

	return mutation{
		record: prepared, previous: previous, deleted: false,
		refreshHash: full && store.copiedHashes != nil, excluded: false,
	}, err
}

func (store *Store) finishScan(
	ctx context.Context,
	generation uint64,
	result Result,
	actor string,
	full bool,
	scopes []string,
	matcher *exclude.Matcher,
	unstable []string,
) (Result, error) {
	// One ordered pass, rather than restarting at the first surviving entry for each deletion batch.
	batch := store.newBatcher(generation, &result, actor)

	err := store.scanMarkers(ctx, scopes, func(path, data []byte) error {
		if len(data) != numberBytes {
			return fault.New("invalid scan marker")
		}

		if binary.BigEndian.Uint64(data) == generation || pathutil.Within(unstable, string(path)) {
			return nil
		}

		return batch.add(ctx, mutation{
			refreshHash: false,
			excluded:    matcher.Match(string(path)),
			record:      integrity.Record{Path: slices.Clone(path)},
			previous:    nil,
			deleted:     true,
		})
	})
	if err != nil {
		return result, err
	}

	err = batch.flush(ctx)
	if err != nil {
		return result, err
	}
	// A subtree cannot certify completeness or hash freshness of the whole inventory.
	if len(scopes) != 0 || len(unstable) != 0 {
		return result, nil
	}

	return result, store.completeScan(full)
}

func (store *Store) completeScan(full bool) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	transaction.state.Complete = true
	if full {
		transaction.state.Stats.LastFullScan = time.Now().UnixNano()
	}

	return transaction.commit()
}

func (store *Store) scanMarkers(ctx context.Context, scopes []string, visit func([]byte, []byte) error) error {
	if len(scopes) == 0 {
		return store.iterate(ctx, seenPrefix, visit)
	}

	for _, scope := range scopes {
		err := store.iterate(ctx, seenPrefix+scope, func(suffix, data []byte) error {
			// A scope named /a must never include siblings such as /ab.
			if len(suffix) != 0 && !strings.HasPrefix(string(suffix), "/") {
				return nil
			}

			return visit(append([]byte(scope), suffix...), data)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (store *Store) prepare(
	ctx context.Context,
	scanner *integrity.Scanner,
	record integrity.Record,
	full bool,
) (integrity.Record, *storedRecord, error) {
	if record.Type != integrity.TypeRegular || full || !slices.Contains(store.Policy.Compare, fieldHash) {
		return record, nil, nil
	}

	previous, data, err := readRecord(store.database, currentPrefix, record.Path)
	if err != nil {
		return record, nil, err
	}

	cached := &storedRecord{record: previous, data: data}
	exists := data != nil

	unchanged := exists &&
		len(
			integrity.Differences(
				previous,
				record,
				[]string{fieldSize, fieldMtime, fieldCtime, fieldInode, fieldDevice, fieldType},
			),
		) == 0 &&
		previous.Algorithm == store.Policy.Hash.Algorithm
	if unchanged {
		record.Hash, record.Algorithm = previous.Hash, previous.Algorithm
		record.HashedAt, record.NoAtime = previous.HashedAt, previous.NoAtime

		return record, cached, nil
	}

	if exists && !store.Policy.Hash.OnEvent {
		// Metadata now describes another version. Its content remains unverified
		// until a full scan; the old digest is not evidence for this observation.
		record.Hash, record.Algorithm = "", ""
		record.HashedAt, record.NoAtime = 0, false

		return record, cached, nil
	}

	record, err = scanner.Observe(ctx, string(record.Path), store.Policy.Hash.Algorithm, true, store.copiedHashes)

	return record, cached, err
}

// Observe updates affected paths, limiting directory reconciliation to that subtree.
func (store *Store) Observe(
	ctx context.Context,
	scanner *integrity.Scanner,
	paths, roots []string,
	matcher *exclude.Matcher,
	actor, identifier string,
) (Result, error) {
	store.operation.Lock()
	defer store.operation.Unlock()

	rootErr := scanner.CheckRoots(roots)
	if rootErr != nil {
		return Result{}, rootErr
	}

	operationErr2 := store.beginOperation(ctx, false)
	if operationErr2 != nil {
		return Result{}, operationErr2
	}

	return store.observePaths(ctx, scanner, paths, roots, matcher, actor, observationResult(identifier))
}

func observationResult(identifier string) Result {
	if identifier == "" {
		identifier = changeID()
	}

	return Result{ID: identifier}
}

func (store *Store) observePaths(
	ctx context.Context,
	scanner *integrity.Scanner,
	paths, roots []string,
	matcher *exclude.Matcher,
	actor string,
	result Result,
) (Result, error) {
	var (
		directories []string
		failure     error
	)

	batch := store.newBatcher(0, &result, actor)

	for _, path := range paths {
		if matcher.Match(path) {
			continue
		}

		update, reconcile, err := store.readAffected(ctx, scanner, path)
		failure = errors.Join(failure, err)

		_, fatal := integrity.SplitUnstable(err)
		if fatal != nil {
			// Keep earlier valid observations when a later path cannot be read.
			flushErr := batch.flush(ctx)

			return result, errors.Join(failure, flushErr)
		}

		if err != nil {
			continue
		}

		if reconcile {
			directories = append(directories, path)

			continue
		}

		err = batch.addObservation(ctx, update)
		if err != nil {
			return result, err
		}
	}

	err := batch.flush(ctx)
	if err != nil {
		return result, err
	}

	result, err = store.observeDirectories(ctx, scanner, directories, roots, matcher, actor, result)

	return result, errors.Join(failure, err)
}

func (store *Store) observeDirectories(
	ctx context.Context,
	scanner *integrity.Scanner,
	directories, roots []string,
	matcher *exclude.Matcher,
	actor string,
	result Result,
) (Result, error) {
	if len(directories) == 0 {
		return result, nil
	}

	// Share one scan generation across the affected subtrees. Explicit file events
	// are still observed separately: they may require hashing despite equal metadata.
	subtrees, err := store.reconcile(
		ctx, scanner, roots, matcher, false, actor, result.ID, pathutil.CompactRoots(directories)...,
	)
	result.Changed += subtrees.Changed
	result.Violations += subtrees.Violations

	return result, err
}

// batcher applies mutations in bounded transactions and accumulates their outcome.
type batcher struct {
	store      *Store
	result     *Result
	actor      string
	pending    []mutation
	generation uint64
}

func (store *Store) newBatcher(generation uint64, result *Result, actor string) *batcher {
	return &batcher{
		store: store, result: result, actor: actor, generation: generation,
		pending: make([]mutation, 0, transactionBatchSize),
	}
}

func (batch *batcher) add(ctx context.Context, update mutation) error {
	batch.pending = append(batch.pending, update)
	if len(batch.pending) < transactionBatchSize {
		return nil
	}

	return batch.flush(ctx)
}

// flush reports cancellation even when nothing is pending, so callers need no separate check.
func (batch *batcher) flush(ctx context.Context) error {
	if len(batch.pending) == 0 {
		return fault.Wrap("apply observations", ctx.Err())
	}

	changed, violations, err := batch.store.apply(ctx, batch.pending, batch.generation, batch.result.ID, batch.actor)
	batch.result.Changed += changed
	batch.result.Violations += violations
	batch.pending = batch.pending[:0]

	return err
}

// addObservation removes descendants of missing names and non-directory replacements.
func (batch *batcher) addObservation(ctx context.Context, update mutation) error {
	if !update.deleted && update.record.Type == integrity.TypeDirectory {
		return batch.add(ctx, update)
	}

	prefix := string(update.record.Path) + "/"
	// Only pending descendants require a flush; ordinary file events retain batching.
	for _, pending := range batch.pending {
		if strings.HasPrefix(string(pending.record.Path), prefix) {
			err := batch.flush(ctx)
			if err != nil {
				return err
			}

			break
		}
	}

	err := batch.add(ctx, update)
	if err != nil {
		return err
	}

	return batch.store.iterate(ctx, currentPrefix+prefix, func(suffix, _ []byte) error {
		return batch.add(ctx, mutation{
			refreshHash: false,
			excluded:    false,
			record:      integrity.Record{Path: append([]byte(prefix), suffix...)},
			previous:    nil,
			deleted:     true,
		})
	})
}

func (store *Store) generation() uint64 {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	return store.state.Generation
}

func (store *Store) readAffected(
	ctx context.Context,
	scanner *integrity.Scanner,
	path string,
) (mutation, bool, error) {
	content := store.Policy.Hash.OnEvent && slices.Contains(store.Policy.Compare, fieldHash)

	record, err := scanner.Observe(ctx, path, store.Policy.Hash.Algorithm, content, store.copiedHashes)
	if integrity.Missing(err) {
		return mutation{
			record:      integrity.Record{Path: []byte(path)},
			deleted:     true,
			previous:    nil,
			excluded:    false,
			refreshHash: false,
		}, false, nil
	}

	if err != nil {
		return mutation{}, false, err
	}

	if record.Type == integrity.TypeDirectory {
		return mutation{}, true, nil
	}

	update := mutation{
		record:      record,
		deleted:     false,
		previous:    nil,
		excluded:    false,
		refreshHash: false,
	}
	if !store.Policy.Hash.OnEvent {
		update.record, update.previous, err = store.prepare(ctx, scanner, record, false)
	}

	return update, false, err
}
