package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/fault"
	gitrepo "github.com/inode64/fsledger/internal/git"
	"github.com/inode64/fsledger/internal/inbound"
)

func (w *worker) incomingActive() bool { return w.incoming != nil && w.incoming.Conflict == "" }

func (w *worker) syncRemote(ctx context.Context, now time.Time) {
	if !w.cfg.Storage.Git.Bidirectional {
		w.publish(ctx, now)

		return
	}

	if w.repo == nil || w.incomingActive() {
		return
	}

	if now.Before(w.fetchNext) {
		if w.status.SyncBlocked == "" && w.status.SyncConflict == "" {
			w.publish(ctx, now)
		}

		return
	}

	settings := w.cfg.Storage.Git
	fetch, cancel := context.WithTimeout(ctx, settings.FetchTimeout)
	remote, err := w.repo.FetchBranch(fetch, settings.Remote, settings.Branch)

	cancel()

	if err != nil {
		w.fetchDelay = max(settings.FetchInterval, min(w.fetchDelay+w.fetchDelay, maximumPublishBackoff))
		w.fetchNext = now.Add(w.fetchDelay)
		w.status.LastFetchError = err.Error()

		return
	}

	w.fetchDelay = 0
	w.fetchNext = now.Add(settings.FetchInterval)
	w.status.RemoteCommit, w.status.LastFetched = remote, time.Now()
	w.status.LastFetchError = ""

	operation, stop := operationContext(ctx)
	defer stop()

	err = w.syncFetched(operation, remote, now)
	if err != nil {
		w.status.SyncBlocked = err.Error()
		if errors.Is(err, inbound.ErrConflict) {
			w.status.SyncConflict = err.Error()
		}

		w.logger.Warn("incoming synchronization postponed", "error", err)
	}
}

func (w *worker) syncFetched(ctx context.Context, remote string, now time.Time) error {
	if remote == "" {
		w.status.SyncBlocked, w.status.SyncConflict = "", ""
		// The branch may have been deleted after an earlier successful publication.
		w.status.LastPublishedCommit = ""
		w.publish(ctx, now)

		return nil
	}

	local, err := w.repo.Head(ctx)
	if err != nil {
		return err
	}

	if local == remote {
		w.status.SyncBlocked, w.status.SyncConflict = "", ""

		return nil
	}

	localAhead, err := w.repo.IsAncestor(ctx, remote, local)
	if err != nil {
		return err
	}

	if localAhead {
		w.status.SyncConflict, w.status.SyncBlocked = "", ""
		w.status.LastPublishedCommit = ""
		w.publish(ctx, now)

		return nil
	}

	remoteAhead, err := w.repo.IsAncestor(ctx, local, remote)
	if err != nil {
		return err
	}

	if !remoteAhead {
		w.status.SyncBlocked = ""
		w.status.SyncConflict = "local and remote histories have diverged; automatic merge is disabled"

		return nil
	}

	return w.beginIncoming(ctx, local, remote)
}

func (w *worker) beginIncoming(ctx context.Context, base, target string) error {
	if w.incoming != nil && w.incoming.Target == target && w.incoming.Conflict != "" {
		w.status.SyncConflict = w.incoming.Conflict

		return nil
	}

	if !w.incomingSourcesReady() {
		return fault.New("source selection is changing, incomplete or unavailable")
	}

	groups, _ := w.manager.Pending()
	if groups != 0 || w.dirty || len(w.deferred) > 0 || len(w.unstable) > 0 {
		return fault.New("local observations or deferred versions are pending")
	}

	err := w.reconcile(ctx, reconciliationMessage("before incoming synchronization"))
	if err != nil {
		return err
	}

	local, err := w.repo.Head(ctx)
	if err != nil {
		return err
	}

	if local != base || len(w.deferred) > 0 || len(w.unstable) > 0 {
		return fault.New("local sources changed before incoming synchronization")
	}

	return w.prepareIncoming(ctx, base, target)
}

func (w *worker) incomingSourcesReady() bool {
	sources := w.cfg.ResolveRepository(w.name)

	return !w.updateSources(sources) && !sources.Incomplete && len(w.status.UnavailableSources) == 0
}

func (w *worker) prepareIncoming(ctx context.Context, base, target string) error {
	status, err := w.repo.Status(ctx)
	if err != nil {
		return err
	}

	if status != "" {
		return fault.New("incoming synchronization requires a clean mirror")
	}

	differences, err := w.repo.TreeChanges(ctx, base, target)
	if err != nil {
		return err
	}

	policy, err := w.incomingPolicy()
	if err != nil {
		return err
	}

	var plan *inbound.Plan

	err = w.mirrorOperation(ctx, func() error {
		var prepareErr error

		plan, prepareErr = inbound.Prepare(ctx, w.repo, w.roots, w.matcher, base, target, policy, differences)

		return prepareErr
	})
	if err != nil {
		return err
	}

	err = w.repo.PinIncoming(ctx, target)
	if err != nil {
		return err
	}

	err = w.catalog.SaveIncoming(ctx, plan)
	if err != nil {
		return err
	}

	w.incoming = plan
	w.status.InboundPending = target
	w.dirty, w.contaminated = true, true
	w.manager.Due(time.Now(), true)

	return w.resumeIncoming(ctx)
}

func (w *worker) incomingPolicy() (string, error) {
	data, err := yaml.Marshal(struct {
		Excludes   []string          `yaml:"excludes"`
		Internal   []string          `yaml:"internal"`
		Repository config.Repository `yaml:"repository"`
	}{w.cfg.Exclude, w.cfg.InternalPaths(), w.cfg.Repositories[w.name]})
	if err != nil {
		return "", fault.Wrap("encode incoming policy", err)
	}

	digest := sha256.Sum256(data)

	return hex.EncodeToString(digest[:]), nil
}

func (w *worker) resumeIncoming(ctx context.Context) error {
	if !w.incomingActive() {
		if w.incoming != nil {
			w.status.SyncConflict = w.incoming.Conflict
		}

		return nil
	}

	w.status.InboundPending = w.incoming.Target

	err := w.applyIncoming(ctx)
	if err == nil {
		return nil
	}

	if errors.Is(err, inbound.ErrConflict) {
		w.incoming.Conflict = err.Error()

		saveErr := w.catalog.SaveIncoming(ctx, w.incoming)
		if saveErr != nil {
			w.incoming.Conflict = ""

			return saveErr
		}

		w.status.SyncConflict = err.Error()
		w.status.InboundPending = ""
		w.dirty, w.contaminated = true, true

		return nil
	}

	return err
}

func (w *worker) applyIncoming(ctx context.Context) error {
	policy, err := w.incomingPolicy()
	if err != nil {
		return err
	}

	if !w.cfg.Storage.Git.Bidirectional || policy != w.incoming.Policy || w.repo == nil {
		return incomingConflict("configuration changed while incoming operation was pending")
	}

	head, err := w.repo.Head(ctx)
	if err != nil {
		return err
	}

	if head != w.incoming.Base && head != w.incoming.Target {
		return incomingConflict("local HEAD changed while incoming operation was pending")
	}

	err = w.mirrorOperation(ctx, func() error {
		return w.incoming.Apply(ctx, w.repo, func() error { return w.catalog.SaveIncoming(ctx, w.incoming) })
	})
	if err != nil {
		return err
	}

	err = w.mirrorOperation(ctx, func() error { return w.mirror.ReconcileContext(ctx) })
	if err != nil {
		return err
	}

	err = w.repo.AdoptIncoming(ctx, head, w.incoming.Target)
	if errors.Is(err, gitrepo.ErrIncomingTreeChanged) {
		return errors.Join(inbound.ErrConflict, err)
	}

	if err != nil {
		return err
	}

	return w.finishIncoming(ctx)
}

func (w *worker) finishIncoming(ctx context.Context) error {
	message := "git-remote:" + w.incoming.Target

	result, err := w.catalog.Reconcile(ctx, w.scanner, w.roots, w.matcher, true, message, "")
	if err != nil {
		return err
	}

	w.rememberChange(result)

	err = w.catalog.ClearOperation(ctx)
	if err != nil {
		return err
	}

	err = w.catalog.SaveIncoming(ctx, nil)
	if err != nil {
		return err
	}

	w.recordCommit(ctx, w.incoming.Target)
	w.status.InboundApplied = w.incoming.Target
	w.status.InboundPending, w.status.SyncBlocked, w.status.SyncConflict = "", "", ""
	w.incoming = nil
	w.dirty, w.contaminated = true, true
	w.manager.Due(time.Now(), true)
	w.logger.Info("incoming synchronization completed", "commit", w.status.InboundApplied)

	return nil
}

func incomingConflict(message string) error {
	return errors.Join(inbound.ErrConflict, fault.New(message))
}
