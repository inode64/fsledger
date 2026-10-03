// Package daemon coordinates independent repository pipelines.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/inode64/fsledger/internal/aisummary"
	"github.com/inode64/fsledger/internal/catalog"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/filter"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/notify"
	"github.com/inode64/fsledger/internal/reporting"
	"github.com/inode64/fsledger/internal/resource"

	"github.com/inode64/fsledger/internal/attribution/audit"
	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/exclude"
	gitrepo "github.com/inode64/fsledger/internal/git"
	"github.com/inode64/fsledger/internal/grouping"
	"github.com/inode64/fsledger/internal/inbound"
	"github.com/inode64/fsledger/internal/mirror"
	"github.com/inode64/fsledger/internal/watcher"
	"github.com/inode64/fsledger/internal/watcher/fanotify"
)

const (
	gitOperationTimeout = 30 * time.Second
	// The final reconciliation of 50+ repositories shares one scanner and needs about three minutes. It is
	// measured from the stop request, including any operation grace, so everything ends before the 300 s
	// that the OpenRC script and the systemd unit wait before SIGKILL.
	shutdownGrace      = 270 * time.Second
	eventQueueSize     = 8192
	schedulingInterval = 250 * time.Millisecond
)

type worker struct {
	gitStatusNext    time.Time
	retryAt          time.Time
	publishNext      time.Time
	fetchNext        time.Time
	unstableRetry    time.Time
	lastPrune        time.Time
	logger           *slog.Logger
	queue            *watcher.Queue
	scanningUnstable map[string]struct{}
	unstable         map[string]struct{}
	incoming         *inbound.Plan
	stopForwarding   func()
	probePIDFD       pidfdProbe
	deferred         map[string]catalog.Deferred
	audit            *audit.Provider
	scanner          *integrity.Scanner
	cfg              *config.Config
	catalog          *catalog.Store
	matcher          *exclude.Matcher
	deferMatcher     *filter.Matcher
	repo             *gitrepo.Repository
	manager          *grouping.Manager
	mirror           *mirror.Sync
	scans            *scanSchedule
	ai               *aisummary.Session
	published        *activeRoots
	name             string
	deferPolicy      string
	watchers         []watcher.Watcher
	errorReasons     []string
	advisories       []string
	roots            []string
	nextRoots        []string
	status           Status
	publishDelay     time.Duration
	fetchDelay       time.Duration
	retryDelay       time.Duration
	// stopRequested holds the UnixNano time when shutdown began; zero until then.
	stopRequested     atomic.Int64
	fullScanRequested bool
	forceCommit       bool
	stopping          bool
	// interrupted marks an operation cut short by shutdown, which the next startup reconciliation resumes.
	interrupted    bool
	initial        bool
	announcedError bool
	pendingError   bool
	dirty          bool
	forceReconcile bool
	contaminated   bool
	sourcesPending bool
}

// Run starts repository workers and waits for bounded graceful shutdown. Sources are resolved here and
// re-read by each worker, so absent entries never prevent starting.
func Run(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	err := preparePaths(cfg)
	if err != nil {
		return err
	}

	scanner := integrity.NewScanner(cfg.Scan.Workers, cfg.Scan.MaxConcurrent)
	sender := notify.New(cfg.Templates)
	summaries := aisummary.New(cfg.AI)
	probe := sharedPIDFDProbe(fanotify.ProbePIDFDLifetime)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	matchers, err := repositoryMatchers(cfg)
	if err != nil {
		return err
	}

	resolved := cfg.ResolveSources()
	roots := publishRoots(resolved)

	auditProvider := startAudit(ctx, cfg, logger, matchers, roots)
	if auditProvider != nil {
		defer resource.Close(auditProvider)
	}

	failures := make(chan error, len(cfg.Repositories))

	var wait sync.WaitGroup
	for _, name := range cfg.Names() {
		wait.Go(func() {
			err := runRepository(
				ctx,
				cfg.ForRepository(name),
				name,
				logger,
				scanner,
				sender,
				auditProvider,
				summaries,
				matchers[name], roots[name], resolved[name], probe,
			)
			if err != nil && (ctx.Err() == nil || !errors.Is(err, ctx.Err())) {
				failures <- fmt.Errorf("repository %s: %w", name, err)

				cancel()
			}
		})
	}

	wait.Wait()
	close(failures)

	var result error
	for failure := range failures {
		result = errors.Join(result, failure)
	}

	return result
}

func runRepository(
	ctx context.Context,
	cfg *config.Config,
	name string,
	logger *slog.Logger,
	scanner *integrity.Scanner,
	sender *notify.Sender,
	auditProvider *audit.Provider,
	summaries *aisummary.Manager,
	matcher *exclude.Matcher,
	published *activeRoots,
	sources config.Sources,
	probe pidfdProbe,
) error {
	lock, err := resource.LockDirectory(cfg.RepositoryPath(name))
	if err != nil {
		return err
	}
	defer resource.Close(lock)

	repo, syncer, initial, err := openBackend(ctx, cfg, name, matcher, published.load())
	if err != nil {
		return err
	}

	if syncer != nil {
		defer resource.Close(syncer)
	}

	store, err := catalog.OpenRepository(ctx, cfg, name)
	if err != nil {
		return err
	}
	defer resource.Close(store)

	deliveryCtx, cancelDelivery := context.WithCancel(ctx)

	var delivery sync.WaitGroup

	delivery.Go(func() { sender.Run(deliveryCtx, store, cfg.Notifiers, logger) })

	delivery.Go(func() { reporting.Run(deliveryCtx, store, cfg.Reports, summaries, logger) })
	defer delivery.Wait()
	defer cancelDelivery()

	runner := newWorker(cfg, name, logger, scanner, store, auditProvider, repo, syncer, matcher, published, probe)
	runner.recordSources(sources)

	err = runner.prepareCommit(ctx, summaries)
	if err != nil {
		return err
	}

	return runner.run(ctx, initial)
}

func (w *worker) prepareCommit(ctx context.Context, summaries *aisummary.Manager) error {
	repo := w.cfg.Repositories[w.name]
	if repo.AIEnabled() {
		session, err := summaries.Session(&repo)
		if err != nil {
			return err
		}

		w.ai = session
	}

	err := w.loadDeferral(ctx)
	if err != nil {
		return err
	}

	w.incoming, err = w.catalog.Incoming(ctx)

	return err
}

func newWorker(
	cfg *config.Config,
	name string,
	logger *slog.Logger,
	scanner *integrity.Scanner,
	store *catalog.Store,
	auditProvider *audit.Provider,
	repo *gitrepo.Repository,
	syncer *mirror.Sync,
	matcher *exclude.Matcher,
	published *activeRoots,
	probe pidfdProbe,
) *worker {
	if syncer != nil {
		store.BindCopiedHashes(syncer.CopiedHash)
	}

	return &worker{
		probePIDFD: probe,
		// Forwarding is lightweight; saturation is sticky even if the event queue is full.
		queue:   watcher.NewQueue(eventQueueSize),
		catalog: store, scanner: scanner,
		audit:          auditProvider,
		cfg:            cfg,
		name:           name,
		repo:           repo,
		mirror:         syncer,
		matcher:        matcher,
		manager:        grouping.New(cfg.Commit.Grouping, cfg.Commit.Debounce, cfg.Commit.MaxDelay),
		logger:         logger.With("repository", name),
		roots:          published.load(),
		published:      published,
		stopForwarding: func() {},
		status: Status{
			Repository: name, Type: cfg.Repositories[name].Type, Paths: published.load(), Running: true,
		},
	}
}

func (w *worker) run(ctx context.Context, initial bool) error {
	w.initial = initial || w.catalog.InitialSnapshotPending()

	stopped := context.AfterFunc(ctx, func() { w.stopRequested.Store(time.Now().UnixNano()) })
	defer stopped()

	// Event delivery is enabled before snapshotting to close the startup gap. Shutdown closes the watchers.
	w.startWatchers(ctx)

	err := w.prepareScanSchedule(time.Now())
	if err != nil {
		return err
	}

	// Journal recovery is mandatory work, independent of optional startup scans.
	if !w.cfg.Watch.Reconcile.OnStart && !w.incomingActive() {
		w.status.Scan.Result = "waiting"
		w.saveStatus(ctx)

		return w.loop(ctx)
	}

	message := reconciliationMessage("startup")
	if initial {
		message = "Initial snapshot for " + w.cfg.Server.Name
	}

	operation, cancel := operationContext(ctx)
	err = w.reconcile(operation, message)
	interrupted := w.interruptedByStop(operation, err)

	cancel()

	if err != nil && !interrupted {
		w.failedReconciliation(err)
		w.saveStatus(ctx)
	}

	return w.loop(ctx)
}

func openBackend(
	ctx context.Context,
	cfg *config.Config,
	name string,
	matcher *exclude.Matcher,
	roots []string,
) (*gitrepo.Repository, *mirror.Sync, bool, error) {
	root := cfg.RepositoryPath(name)

	err := config.CheckRepositoryStorage(root, cfg.Repositories[name].Type)
	if err != nil {
		return nil, nil, false, err
	}

	if cfg.Repositories[name].Type != config.RepositoryGit {
		return nil, nil, false, nil
	}

	repo := &gitrepo.Repository{Path: root, Host: cfg.Server.Name, Timeout: cfg.Storage.Git.Timeout}

	initial, err := repo.Init(context.WithoutCancel(ctx))
	if err != nil {
		return nil, nil, false, err
	}

	syncer, err := mirror.Open(root, roots, matcher, mirrorAlgorithm(cfg))
	if err != nil {
		return nil, nil, false, err
	}

	return repo, syncer, initial, nil
}

func mirrorAlgorithm(cfg *config.Config) string {
	if !slices.Contains(cfg.Integrity.Compare, "hash") {
		return ""
	}

	return cfg.Integrity.Hash.Algorithm
}

func repositoryMatchers(cfg *config.Config) (map[string]*exclude.Matcher, error) {
	matchers := make(map[string]*exclude.Matcher, len(cfg.Repositories))
	for _, name := range cfg.Names() {
		matcher, err := cfg.Matcher(name)
		if err != nil {
			return nil, err
		}

		matchers[name] = matcher
	}

	return matchers, nil
}

func preparePaths(cfg *config.Config) error {
	// The log destination was inspected when it was opened; only state locations remain.
	checkPathsErr := cfg.CheckStoragePaths()
	if checkPathsErr != nil {
		return fault.Wrap("daemon operation", checkPathsErr)
	}

	for _, path := range []string{cfg.Storage.Path, cfg.Runtime} {
		err := os.MkdirAll(path, 0o700)
		if err != nil {
			return fault.Wrap("daemon operation", err)
		}
	}

	return nil
}
