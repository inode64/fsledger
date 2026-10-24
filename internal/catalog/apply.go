package catalog

import (
	"bytes"
	"context"
	"encoding/binary"
	"slices"
	"time"

	"github.com/inode64/fsledger/internal/config"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/filter"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/resource"
)

func (store *Store) apply(
	ctx context.Context,
	updates []mutation,
	generation uint64,
	identifier, actor string,
) (int, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	changed := 0
	seen := make(map[string]bool, len(updates))

	for _, update := range updates {
		operationErr1 := ctx.Err()
		if operationErr1 != nil {
			return 0, fault.Wrap("apply inventory", operationErr1)
		}

		// Repeated paths must see preceding writes in this indexed batch.
		path := string(update.record.Path)
		if seen[path] {
			update.previous = nil
		}

		seen[path] = true

		didChange, err := store.applyOne(ctx, transaction, update, generation, identifier, actor)
		if err != nil {
			return 0, err
		}

		if didChange {
			changed++
		}
	}

	return changed, transaction.commit()
}

func differences(
	previous, current integrity.Record,
	previousExists, currentExists bool,
	policy config.Integrity,
) []string {
	if !previousExists && !currentExists {
		return nil
	}

	if !previousExists {
		return []string{changeAdded}
	}

	if !currentExists {
		return []string{changeDeleted}
	}

	fields := integrity.Differences(previous, current, policy)
	if current.Type == integrity.TypeRegular && current.Hash == "" {
		// A deferred hash is unknown, not a content difference.
		fields = slices.DeleteFunc(fields, func(field string) bool { return field == fieldHash })
	}

	return fields
}

// verifiedFields drops a hash difference from a version that was never hashed: hashing the new version
// cannot verify the old content. Hash refresh for Git still uses every difference.
func verifiedFields(previous integrity.Record, fields []string) []string {
	if previous.Type != integrity.TypeRegular || previous.Hash != "" {
		return fields
	}

	return slices.DeleteFunc(slices.Clone(fields), func(field string) bool { return field == fieldHash })
}

func changeKind(fields []string) string {
	if slices.Contains(fields, changeAdded) {
		return changeAdded
	}

	if slices.Contains(fields, changeDeleted) {
		return changeDeleted
	}

	return changeModified
}

type comparison struct {
	oldData     []byte
	currentData []byte
	fields      []string
	previous    integrity.Record
}

func compare(transaction *transaction, update mutation, policy config.Integrity) (comparison, error) {
	var (
		result comparison
		err    error
	)

	record := update.record
	if update.previous != nil {
		result.previous, result.oldData = update.previous.record, update.previous.data
	} else {
		result.previous, result.oldData, err = readRecord(transaction.batch, currentPrefix, record.Path)
		if err != nil {
			return result, err
		}
	}

	if !update.deleted {
		result.currentData, err = packObservation(record, result.previous, result.oldData)
		if err != nil {
			return result, err
		}
	}

	result.fields = differences(result.previous, record, result.oldData != nil, result.currentData != nil, policy)

	return result, nil
}

func (store *Store) applyOne(
	ctx context.Context,
	transaction *transaction,
	update mutation,
	generation uint64,
	identifier, actor string,
) (bool, error) {
	update = store.withoutDirectory(update)
	record := update.record

	compared, err := compare(transaction, update, store.policy)
	if err != nil {
		return false, err
	}

	if update.excluded || store.untrackedDirectory(update, &compared) {
		persistEntry(transaction, record.Path, nil, compared.oldData, generation)

		return false, nil
	}

	changed := len(compared.fields) > 0
	if changed {
		err = store.announceChange(ctx, transaction, record, &compared, identifier, actor)
	} else {
		err = refreshStagedObservation(transaction, record, &compared, identifier)
	}

	if err != nil {
		return false, err
	}

	// A hash difference implies both versions exist: additions and deletions carry no field names.
	if update.refreshHash && record.Type == integrity.TypeRegular && slices.Contains(compared.fields, fieldHash) {
		transaction.set(key(hashPrefix, record.Path), []byte(identifier))
	}

	persistEntry(transaction, record.Path, compared.currentData, compared.oldData, generation)

	return changed, nil
}

// ignored is asked only about changes, so unchanged entries of a full scan never reach the rules.
func (store *Store) ignored(ctx context.Context, path []byte) bool {
	return store.ignore.Match(string(path), filter.Actor(ctx, string(path)))
}

// withoutDirectory turns a directory observation into the disappearance of whatever this repository
// stored at that path when directories are not tracked: a stored directory is then retired silently,
// while a file replaced by a directory is still reported as deleted.
func (store *Store) withoutDirectory(update mutation) mutation {
	if store.policy.Directories || update.deleted || update.record.Type != integrity.TypeDirectory {
		return update
	}

	return deletion(update.record.Path, update.excluded)
}

// untrackedDirectory reports a stored directory entry, kept from before directories stopped being
// tracked, that must leave the inventory without any announcement.
func (store *Store) untrackedDirectory(update mutation, compared *comparison) bool {
	if store.policy.Directories || !update.deleted {
		return false
	}

	return compared.oldData != nil && compared.previous.Type == integrity.TypeDirectory
}

func persistEntry(transaction *transaction, path, data, oldData []byte, generation uint64) {
	if data == nil {
		transaction.remove(key(currentPrefix, path))
		transaction.remove(key(seenPrefix, path))
		// A deleted entry has nothing left to refresh, and its path may no longer be a source.
		transaction.remove(key(hashPrefix, path))

		if oldData != nil {
			transaction.state.Stats.Entries--
		}

		return
	}

	if !bytes.Equal(data, oldData) {
		transaction.set(key(currentPrefix, path), data)
	}

	// Events do not start a scan generation. Existing entries retain their marker;
	// scans must still mark every observed entry to make deletion pruning safe.
	if generation != 0 || oldData == nil {
		transaction.set(key(seenPrefix, path), binary.BigEndian.AppendUint64(nil, generation))
	}

	if oldData == nil {
		transaction.state.Stats.Entries++
	}
}

// newDetail describes how fields changed from previous to current, the version at the path; for a
// deletion current is the empty record of that path.
func newDetail(previous, current integrity.Record, fields []string, actor string, observed int64) (Detail, error) {
	changes, err := describeFields(previous, current, fields)
	if err != nil {
		return Detail{}, err
	}

	kind, existing := changeKind(fields), current
	if kind == changeDeleted {
		existing = previous
	}

	return Detail{
		ChangeID:    "",
		Path:        current.Path,
		Kind:        kind,
		Actor:       actor,
		Observed:    observed,
		Fields:      changes,
		BackdatedBy: existing.Backdated(),
	}, nil
}

// observedAt is when an observation was made; one without a time is taken as made now.
func observedAt(record integrity.Record) int64 {
	if record.Observed == 0 {
		return time.Now().UnixNano()
	}

	return record.Observed
}

func describeFields(previous, current integrity.Record, fields []string) ([]FieldChange, error) {
	if len(fields) == 0 {
		return nil, nil
	}

	switch {
	case slices.Contains(fields, changeAdded):
		fields = snapshotFields(current)
	case slices.Contains(fields, changeDeleted):
		fields = snapshotFields(previous)
	}

	result := make([]FieldChange, 0, len(fields))
	for _, field := range fields {
		before, err := printableField(previous, field)
		if err != nil {
			return nil, err
		}

		after, err := printableField(current, field)
		if err != nil {
			return nil, err
		}

		result = append(result, FieldChange{Field: field, Before: before, After: after})
	}

	return result, nil
}

// snapshotFields lists what a created or deleted entry records about the version that exists, leaving
// out values it does not have and attributes its filesystem does not provide: they would only show and
// store empty placeholders.
func snapshotFields(existing integrity.Record) []string {
	fields := []string{fieldType}

	if existing.Hash != "" {
		fields = append(fields, fieldHash)
	}

	fields = append(fields,
		fieldSize, fieldInode, fieldDevice, fieldMode, fieldUID, fieldGID, fieldNlink, fieldMtime, fieldCtime,
	)

	if existing.Atime != 0 {
		fields = append(fields, fieldAtime)
	}

	if existing.HasBtime {
		fields = append(fields, fieldBtime, fieldHasBtime)
	}

	if len(existing.Target) != 0 {
		fields = append(fields, fieldTarget)
	}

	if integrity.AttributesSupported(existing.AttributesStatus) {
		fields = append(fields, fieldXattrs, fieldACL, fieldAttributesStatus)
	}

	return fields
}

// ChangedHashes streams the mirror refreshes pending since the last ClearOperation: every hash change a
// full scan found, which a forced copy brings to the mirror even when the file's signature did not change.
func (store *Store) ChangedHashes(ctx context.Context, visit func(string) error) error {
	return store.iterate(ctx, hashPrefix, func(path, _ []byte) error { return visit(string(path)) })
}

// ClearOperation removes transient hash refresh work after its consumer finishes.
func (store *Store) ClearOperation(ctx context.Context) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	err := ctx.Err()
	if err != nil {
		return fault.Wrap("clear operation", err)
	}

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	transaction.clear(hashPrefix)

	return transaction.commit()
}

func (store *Store) announceChange(
	ctx context.Context,
	transaction *transaction,
	record integrity.Record,
	compared *comparison,
	identifier, actor string,
) error {
	report := store.reports.Enabled
	notifyChange := store.notifies(config.NotificationChange)

	if !report && !notifyChange {
		return nil
	}

	detail, err := newDetail(
		compared.previous, record, verifiedFields(compared.previous, compared.fields), actor, observedAt(record),
	)
	if err != nil {
		return err
	}

	if report {
		reportDetail := detail
		reportDetail.ChangeID = identifier

		err = store.recordReport(transaction, reportDetail)
		if err != nil {
			return err
		}
	}

	if !notifyChange {
		return nil
	}

	return store.enqueueObservedChange(
		transaction,
		record,
		compared,
		detail,
		identifier,
		!store.ignored(ctx, record.Path),
	)
}
