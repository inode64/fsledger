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
) (int, int, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	transaction := store.begin()
	defer resource.Close(transaction.batch)

	if !store.replacingBaseline && store.policy.Reference == config.ReferenceBaseline &&
		transaction.state.Stats.BaselineReady &&
		transaction.state.Policy != store.policyDigest {
		return 0, 0, fault.New("baseline policy differs from configuration")
	}

	changed, violations := 0, 0
	seen := make(map[string]bool, len(updates))

	for _, update := range updates {
		operationErr1 := ctx.Err()
		if operationErr1 != nil {
			return 0, 0, fault.Wrap("apply inventory", operationErr1)
		}

		// Repeated paths must see preceding writes in this indexed batch.
		path := string(update.record.Path)
		if seen[path] {
			update.previous = nil
		}

		seen[path] = true

		didChange, violation, err := store.applyOne(ctx, transaction, update, generation, identifier, actor)
		if err != nil {
			return 0, 0, err
		}

		if didChange {
			changed++
		}

		if violation {
			violations++
		}
	}

	return changed, violations, transaction.commit()
}

func differences(previous, current integrity.Record, previousExists, currentExists bool, selected []string) []string {
	if !previousExists && !currentExists {
		return nil
	}

	if !previousExists {
		return []string{changeAdded}
	}

	if !currentExists {
		return []string{changeDeleted}
	}

	fields := integrity.Differences(previous, current, selected)
	if current.Type == integrity.TypeRegular && current.Hash == "" {
		// A deferred hash is unknown, not a content difference.
		fields = slices.DeleteFunc(fields, func(field string) bool { return field == fieldHash })
	}

	return fields
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
	old            pending
	oldData        []byte
	currentData    []byte
	baselineData   []byte
	fields         []string
	baselineFields []string
	previous       integrity.Record
	baseline       integrity.Record
	pendingExists  bool
}

func (store *Store) compare(transaction *transaction, update mutation) (comparison, error) {
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

	result.fields = differences(
		result.previous,
		record,
		result.oldData != nil,
		result.currentData != nil,
		store.policy.Compare,
	)

	result.old, result.pendingExists, err = readPending(transaction.batch, record.Path)
	if err != nil {
		return result, err
	}

	if transaction.state.Stats.BaselineReady &&
		(store.policy.Reference == config.ReferenceBaseline || store.replacingBaseline) {
		result.baseline, result.baselineData, err = readRecord(
			transaction.batch,
			baselinePrefix(transaction.state.Baseline),
			record.Path,
		)
		if err != nil {
			return result, err
		}

		result.baselineFields = store.baselineDifferences(&result, record)
	}

	return result, nil
}

// baselineDifferences compares an observation with its reference. A deferred hash leaves the content
// unknown: it raises no content difference, but neither resolves one already pending for that path.
func (store *Store) baselineDifferences(compared *comparison, record integrity.Record) []string {
	if compared.baselineData != nil && compared.currentData != nil &&
		compared.pendingExists && slices.Contains(compared.old.Fields, fieldHash) {
		return integrity.Differences(compared.baseline, record, store.policy.Compare)
	}

	return differences(
		compared.baseline,
		record,
		compared.baselineData != nil,
		compared.currentData != nil,
		store.policy.Compare,
	)
}

func (store *Store) applyOne(
	ctx context.Context,
	transaction *transaction,
	update mutation,
	generation uint64,
	identifier, actor string,
) (bool, bool, error) {
	update = store.withoutDirectory(update)
	record := update.record

	compared, err := store.compare(transaction, update)
	if err != nil {
		return false, false, err
	}

	if update.excluded || store.untrackedDirectory(update, &compared) {
		retireEntry(transaction, record.Path, &compared, generation)

		return false, false, nil
	}

	changed := len(compared.fields) > 0
	violation := len(compared.baselineFields) > 0 &&
		(!compared.pendingExists || changed || !slices.Equal(compared.old.Fields, compared.baselineFields))

	token := store.setPending(
		transaction, record.Path, compared.old, compared.pendingExists, identifier,
		changeKind(compared.baselineFields), compared.baselineFields, violation,
	)

	err = store.announceChange(
		ctx,
		transaction,
		record,
		&compared,
		identifier,
		token,
		actor,
		changed,
		violation,
	)
	if err != nil {
		return false, false, err
	}

	err = store.refreshStagedObservation(transaction, record, &compared, identifier)
	if err != nil {
		return false, false, err
	}

	// A hash difference implies both versions exist: additions and deletions carry no field names.
	if update.refreshHash && record.Type == integrity.TypeRegular && slices.Contains(compared.fields, fieldHash) {
		transaction.set(key(hashPrefix, record.Path), []byte(identifier))
	}

	persistEntry(transaction, record.Path, compared.currentData, compared.oldData, generation)

	return changed, violation, nil
}

// ignored is asked only about changes, so unchanged entries of a full scan never reach the rules.
func (store *Store) ignored(ctx context.Context, path []byte) bool {
	return store.ignore.Match(string(path), filter.Actor(ctx, string(path)))
}

// retireEntry stops tracking a path without announcing anything: exclusions and untracked directories.
func retireEntry(transaction *transaction, path []byte, compared *comparison, generation uint64) {
	if compared.pendingExists {
		removePending(transaction, path, compared.old, true)
	}

	persistEntry(transaction, path, nil, compared.oldData, generation)
}

// withoutDirectory turns a directory observation into the disappearance of whatever this repository
// stored at that path when directories are not tracked: a stored directory is then retired silently,
// while a file replaced by a directory is still reported as deleted.
func (store *Store) withoutDirectory(update mutation) mutation {
	if store.policy.Directories || update.deleted || update.record.Type != integrity.TypeDirectory {
		return update
	}

	return mutation{
		record:      integrity.Record{Path: update.record.Path},
		previous:    nil,
		deleted:     true,
		excluded:    update.excluded,
		refreshHash: false,
	}
}

// untrackedDirectory reports a stored directory entry, kept from before directories stopped being
// tracked or from the approved reference, that must leave the inventory without any announcement.
func (store *Store) untrackedDirectory(update mutation, compared *comparison) bool {
	if store.policy.Directories || !update.deleted {
		return false
	}

	if compared.oldData != nil {
		return compared.previous.Type == integrity.TypeDirectory
	}

	return compared.baseline.Type == integrity.TypeDirectory
}

func persistEntry(transaction *transaction, path, data, oldData []byte, generation uint64) {
	if data == nil {
		transaction.remove(key(currentPrefix, path))
		transaction.remove(key(seenPrefix, path))

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

func describe(record integrity.Record, compared *comparison, actor string) (Detail, error) {
	fields, err := describeFields(compared.previous, record, compared.fields)
	if err != nil {
		return Detail{}, err
	}

	baseline, err := describeFields(compared.baseline, record, compared.baselineFields)
	if err != nil {
		return Detail{}, err
	}

	observed := record.Observed
	if observed == 0 {
		observed = time.Now().UnixNano()
	}

	return Detail{
		ChangeID: "",
		Path:     record.Path,
		Kind:     changeKind(compared.fields),
		Actor:    actor,
		Observed: observed,
		Fields:   fields,
		Baseline: baseline,
	}, nil
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
// out attributes its filesystem does not provide: they would only show and store empty placeholders.
func snapshotFields(existing integrity.Record) []string {
	fields := []string{
		fieldType, fieldHash, fieldSize, fieldInode, fieldDevice, fieldMode, fieldUID, fieldGID, fieldNlink,
		fieldMtime, fieldCtime,
	}

	if existing.Atime != 0 {
		fields = append(fields, fieldAtime)
	}

	if existing.HasBtime {
		fields = append(fields, fieldBtime, fieldHasBtime)
	}

	fields = append(fields, fieldTarget)

	if integrity.AttributesSupported(existing.AttributesStatus) {
		fields = append(fields, fieldXattrs, fieldACL, fieldAttributesStatus)
	}

	return fields
}

// ChangedHashes streams temporary work for the current Git reconciliation.
func (store *Store) ChangedHashes(ctx context.Context, identifier string, visit func(string) error) error {
	return store.iterate(ctx, hashPrefix, func(path, operation []byte) error {
		if string(operation) != identifier {
			return nil
		}

		return visit(string(path))
	})
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
	identifier, token, actor string,
	changed, violation bool,
) error {
	report := store.reports.Enabled && (changed || violation)
	reportViolation := violation
	notifyChange := changed && store.notifies(config.NotificationChange)

	var notification changeNotification

	if notifyChange {
		notification.deliver = !store.ignored(ctx, record.Path)
		notification.stage = store.notificationOperation == identifier
	}

	violation = violation && store.notifies(config.NotificationIntegrityViolation)
	if !report && !notification.deliver && !notification.stage && !violation {
		return nil
	}

	detail, err := describe(record, compared, actor)
	if err != nil {
		return err
	}

	if report {
		reportDetail := detail
		reportDetail.ChangeID = identifier

		err = store.recordReport(transaction, reportDetail, reportViolation)
		if err != nil {
			return err
		}
	}

	err = store.enqueueObservedChange(transaction, record, compared, detail, identifier, notification)
	if err != nil {
		return err
	}

	if violation {
		store.enqueue(transaction, config.NotificationIntegrityViolation, token, []Detail{detail})
	}

	return nil
}
