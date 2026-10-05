package daemon

import (
	"context"
	"crypto/rand"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/inode64/fsledger/internal/config"
	gitrepo "github.com/inode64/fsledger/internal/git"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/filter"
	"github.com/inode64/fsledger/internal/mirror"
)

func (w *worker) beginIntent(ctx context.Context, message string) (string, string, error) {
	identifier := rand.Text()
	if w.repo == nil {
		return identifier, message, nil
	}

	err := w.catalog.Intent(ctx, identifier)

	return identifier, message + "\n" + gitrepo.ChangeIDTrailer + " " + identifier + "\n", err
}

func (w *worker) commit(ctx context.Context, group event.Group) error {
	if w.incomingActive() {
		return fault.New("incoming operation requires recovery before local commits")
	}

	w.scanningUnstable = make(map[string]struct{})
	defer func() { w.scanningUnstable = nil }()

	ctx = filter.WithActors(ctx, func(path string) event.Actor { return groupActor(group, path) })

	paths := make([]string, 0, len(group.Paths))
	for path := range group.Paths {
		paths = append(paths, path)
	}

	sort.Strings(paths)

	identifier, message, err := w.beginIntent(ctx, groupMessage(group, w.cfg.Commit.Grouping.PreferLoginUID))
	if err != nil {
		return err
	}

	err = w.applyGit(ctx, paths, message)
	if err != nil {
		return err
	}

	result, err := w.catalog.Observe(
		ctx,
		w.scanner,
		paths,
		w.roots,
		w.matcher,
		message,
		identifier,
	)

	err = w.deferUnstable(err)
	if err != nil {
		return err
	}

	w.rememberChange(result)

	if w.repo != nil && len(w.scanningUnstable) == 0 {
		return w.catalog.CompleteIntent(ctx, identifier)
	}

	return nil
}

func (w *worker) applyGit(ctx context.Context, paths []string, message string) error {
	if w.repo == nil {
		return nil
	}

	err := w.mirrorOperation(ctx, func() error { return w.mirror.ApplyPaths(ctx, paths) })

	err = w.deferUnstable(err)
	if err != nil {
		return fmt.Errorf("mirror event group: %w", err)
	}

	w.checkLosses()

	if w.contaminated {
		return fault.New("event loss during mirror copy; unknown reconciliation required")
	}

	relative := make([]string, 0, len(paths))
	for _, path := range paths {
		name, err := mirror.Relative(path)
		if err != nil {
			return err
		}

		relative = append(relative, name)
	}

	return w.commitGit(ctx, message, relative)
}

func (w *worker) commitGit(ctx context.Context, message string, paths []string) error {
	if len(w.cfg.Commit.Defer) > 0 || len(w.deferred) > 0 || w.ai != nil {
		return w.gateCommit(ctx, message, paths)
	}

	var (
		hash string
		err  error
	)

	if paths == nil {
		hash, err = w.repo.Commit(ctx, message)
	} else {
		hash, err = w.repo.CommitPaths(ctx, message, paths)
	}

	if err != nil {
		return err
	}

	w.recordCommit(ctx, hash)

	return nil
}

func (w *worker) recordCommit(ctx context.Context, hash string) {
	defer func() { w.catalog.ReportHead(w.status.LastCommit) }()

	if hash != "" {
		w.status.LastCommit = hash
		w.logger.Info("commit created", "commit", hash)
	} else if w.status.LastCommit == "" {
		head, headErr := w.repo.Head(ctx)
		if headErr == nil {
			w.status.LastCommit = head
		}
	}
}

func (w *worker) mirrorOperation(ctx context.Context, operation func() error) error {
	return w.scanner.Do(ctx, operation)
}

func (w *worker) reconcileRun(ctx context.Context, message string) error {
	if w.initial {
		err := w.catalog.SetInitialSnapshotPending(ctx, true)
		if err != nil {
			return err
		}
	}

	err := w.resumeIncoming(ctx)
	if err != nil {
		return err
	}

	w.scanningUnstable = make(map[string]struct{})
	defer func() { w.scanningUnstable = nil }()

	w.announceError(ctx)
	w.logger.Info("reconciliation started")
	// Pin catalog-side root identities before Git. Otherwise a failed initial Git
	// commit leaves only the mirror guard initialized and later loss looks like deletion.
	err = w.scanner.CheckRoots(w.roots)
	if err != nil {
		return err
	}

	identifier, message, err := w.beginIntent(ctx, message)
	if err != nil {
		return err
	}

	if w.repo != nil {
		err = w.mirrorOperation(ctx, func() error { return w.mirror.ReconcileContext(ctx) })

		err = w.deferUnstable(err)
		if err != nil {
			return fmt.Errorf("reconcile mirror: %w", err)
		}

		err = w.commitGit(ctx, message, nil)
		if err != nil {
			return err
		}
	}

	err = w.reconcileCatalog(ctx, message, identifier)
	if err != nil {
		return err
	}

	err = w.completeInitialScan(ctx)
	if err != nil {
		return err
	}

	w.finishUnstableScan()
	w.status.LastReconciliation = time.Now()
	w.logger.Info("reconciliation completed", "unstable_paths", len(w.unstable))

	return nil
}

func (w *worker) completeInitialScan(ctx context.Context) error {
	if len(w.scanningUnstable) != 0 {
		return nil
	}

	err := w.announceRecovery(ctx)
	if err != nil || !w.initial {
		return err
	}

	err = w.catalog.SetInitialSnapshotPending(ctx, false)
	if err == nil {
		w.initial = false
	}

	return err
}

func (w *worker) reconcileCatalog(ctx context.Context, message, identifier string) error {
	stats, err := w.catalog.Stats(ctx)
	if err != nil {
		return err
	}

	full := w.fullScanRequested || fullScanDue(w.cfg.Integrity.Hash, stats.LastFullScan, time.Now())
	w.status.Scan.Full = full
	w.saveStatus(ctx)

	result, err := w.catalog.Reconcile(
		ctx, w.scanner, w.roots, w.matcher, full, message, identifier,
	)
	w.status.Scan.Observed, w.status.Scan.Hashed = result.Observed, result.Hashed

	err = w.deferUnstable(err)
	if err != nil {
		return err
	}

	w.rememberChange(result)

	if w.repo != nil && full && result.Changed > 0 {
		err = w.refreshHashes(ctx, result.ID, message)
		if err != nil {
			return err
		}
	}

	if len(w.scanningUnstable) == 0 {
		err = w.recoverIntents(ctx)
		if err != nil {
			return err
		}
	}

	return w.catalog.ClearOperation(ctx)
}

func (w *worker) refreshHashes(ctx context.Context, identifier, message string) error {
	err := w.catalog.ChangedHashes(ctx, identifier, func(path string) error {
		return w.deferUnstable(w.mirrorOperation(ctx, func() error { return w.mirror.RefreshContext(ctx, path) }))
	})
	if err != nil {
		return err
	}

	return w.commitGit(ctx, message, nil)
}

func (w *worker) recoverIntents(ctx context.Context) error {
	pending, err := w.catalog.PendingIntents(ctx)
	if err != nil {
		return err
	}
	// A complete unknown reconciliation closes partial Git operations conservatively.
	return w.catalog.CompleteIntents(ctx, pending)
}

func (w *worker) rememberChange(result catalog.Result) {
	if result.Changed > 0 {
		w.status.LastChange = result.ID
	}
}

// announceRecovery runs after a complete reconciliation. The announcement itself waits for the quiet
// period: a relapse inside it belongs to the error already announced.
func (w *worker) announceRecovery(ctx context.Context) error {
	if !w.announcedError {
		return nil
	}

	w.recovered = true
	w.pendingError = false

	return w.settleRecovery(ctx, time.Now())
}

// settleRecovery delivers a held recovery once no warning has interrupted the quiet period.
func (w *worker) settleRecovery(ctx context.Context, now time.Time) error {
	if !w.recovered || now.Sub(w.lastWarning) < w.recoveryQuiet {
		return nil
	}

	err := w.catalog.Announce(ctx, config.NotificationRecovery, strings.Join(w.errorReasons, "; "))
	if err != nil {
		return err
	}

	w.errorReasons = nil
	w.announcedError = false
	w.recovered = false
	w.refreshWarnings()

	return nil
}
