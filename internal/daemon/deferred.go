package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"time"

	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/filter"
)

// Bounds superseded deferred blobs to roughly one interval of observations.
const deferredPruneInterval = 10 * time.Minute

func (w *worker) loadDeferral(ctx context.Context) error {
	matcher, err := filter.Compile(w.cfg.Commit.Defer)
	if err != nil {
		return err
	}

	data, err := json.Marshal(w.cfg.Commit.Defer)
	if err != nil {
		return fault.Wrap("encode deferral policy", err)
	}

	digest := sha256.Sum256(data)
	w.deferPolicy = hex.EncodeToString(digest[:])
	w.deferMatcher = matcher
	w.deferred, err = w.catalog.Deferred(ctx)

	return err
}

func groupActor(group event.Group, path string) event.Actor {
	if !group.Actor.Known {
		return event.Actor{}
	}

	raw, exists := group.Paths[path]
	if !exists {
		return event.Actor{}
	}

	if raw.Actor.Known {
		return raw.Actor
	}

	return group.Actor
}

func (w *worker) gateCommit(ctx context.Context, message string, paths []string) error {
	err := w.repo.Stage(ctx, paths)
	if err != nil {
		return err
	}

	versions, err := w.repo.StagedVersions(ctx)
	if err != nil {
		return err
	}

	pending, trigger := w.classifyVersions(ctx, versions)
	if !trigger {
		w.recordCommit(ctx, "")

		err = w.saveDeferred(ctx, pending)
		if err == nil {
			w.pruneSuperseded(ctx)
		}

		return err
	}

	accumulated := hasAccumulatedVersions(versions, w.deferred)

	if accumulated || paths == nil {
		message = reconciliationMessage("accumulated changes; triggering observation follows") + "\n\n" + message
	}

	message = w.summarize(ctx, message, versions)

	hash, err := w.repo.CommitVersions(ctx, message, versions)
	if err != nil {
		return err
	}

	w.recordCommit(ctx, hash)
	// A crash after Git commits but before cleanup is safe: the next staged diff
	// no longer contains those versions, and clears their stale pending entries.
	err = w.saveDeferred(ctx, nil)
	if err == nil && accumulated {
		w.pruneSuperseded(ctx)
	}

	return err
}

// The worker owns deferred state; an unchanged selection needs no catalog read.
func (w *worker) saveDeferred(ctx context.Context, pending map[string]catalog.Deferred) error {
	if maps.Equal(w.deferred, pending) {
		return fault.Wrap("save deferred paths", ctx.Err())
	}

	err := w.catalog.SaveDeferred(ctx, pending)
	if err == nil {
		w.deferred = pending
	}

	return err
}

func hasAccumulatedVersions(versions map[string]string, deferred map[string]catalog.Deferred) bool {
	for path := range deferred {
		if _, exists := versions[path]; exists {
			return true
		}
	}

	return false
}

// Pruning only reclaims space: a failure never blocks commits or deferral.
func (w *worker) pruneSuperseded(ctx context.Context) {
	if time.Since(w.lastPrune) < deferredPruneInterval {
		return
	}

	w.lastPrune = time.Now()

	err := w.repo.PruneUnreachable(ctx)
	if err != nil {
		w.logger.Warn("prune superseded deferred versions", "error", err)
	}
}

func (w *worker) classifyVersions(ctx context.Context, versions map[string]string) (map[string]catalog.Deferred, bool) {
	pending := make(map[string]catalog.Deferred, len(versions))

	trigger := w.forceCommit || w.initial
	for path, signature := range versions {
		old, exists := w.deferred[path]
		if exists && old.Signature == signature && old.Policy == w.deferPolicy {
			pending[path] = old

			continue
		}

		if w.deferMatcher.Match(path, filter.Actor(ctx, path)) {
			since := time.Now().UnixNano()
			if exists {
				since = old.Since
			}

			pending[path] = catalog.Deferred{Signature: signature, Policy: w.deferPolicy, Since: since}
		} else {
			trigger = true
		}
	}

	return pending, trigger
}

func (w *worker) summarize(ctx context.Context, message string, versions map[string]string) string {
	if w.ai == nil || w.initial || w.stopping || len(versions) == 0 {
		return message
	}

	summary, status := w.ai.Summarize(ctx, w.repo, versions)
	w.status.AIStatus = status

	if summary == "" {
		return message
	}

	return message + "\n\n" + summary
}
