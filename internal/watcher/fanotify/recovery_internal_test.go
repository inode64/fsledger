package fanotify

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/resource"
	"github.com/inode64/fsledger/internal/watcher"
)

func TestDeletedDescriptorRequestsReconciliationWithoutLoss(t *testing.T) {
	t.Parallel()

	file, err := os.CreateTemp(t.TempDir(), "removed")
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(file)

	err = os.Remove(file.Name())
	if err != nil {
		t.Fatal(err)
	}

	detector := new(Watcher)
	detector.queue = watcher.NewQueue(1)

	err = detector.handleDescriptor(int(file.Fd()), 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	if detector.Dirty() || detector.coverageLost.Load() || !detector.ReconciliationPending() {
		t.Fatal("ordinary unlink was reported as lost watcher coverage")
	}
}

func TestVerifiedMarksAreReusedAndLossCanRecover(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	var stat unix.Stat_t

	err := unix.Lstat(root, &stat)
	if err != nil {
		t.Fatal(err)
	}

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector := new(Watcher)
	detector.fd = -1 // A redundant fanotify_mark syscall would fail this rootless test.
	detector.mode = ModeDFIDName
	detector.matcher = matcher
	detector.queue = watcher.NewQueue(1)
	detector.path = root
	detector.marks.Store(root, directoryIdentity{device: stat.Dev, inode: stat.Ino})
	detector.marks.Store(filepath.Join(root, "vanished"), directoryIdentity{device: stat.Dev, inode: stat.Ino})
	detector.refreshCoverage()

	if _, exists := detector.marks.Load(filepath.Join(root, "vanished")); exists {
		t.Fatal("refresh retained a vanished mark")
	}

	if !detector.Covers(filepath.Join(root, "child")) || detector.Dirty() {
		t.Fatal("verified unchanged directory was marked again")
	}

	detector.loss("simulated overflow")

	if detector.Covers(filepath.Join(root, "child")) || !detector.Dirty() || detector.Dirty() {
		t.Fatal("loss must disable coverage and be consumed exactly once")
	}
	// No native marks are needed for a source that vanished; successful completion
	// must acknowledge this epoch, rather than leaving coverage permanently lost.
	detector.path = filepath.Join(root, "vanished")
	detector.refreshCoverage()

	if detector.coverageLost.Load() {
		t.Fatal("successful refresh never recovered coverage")
	}
}

func TestDirectoryCoverageOnlyQueuesSelectedTrees(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	matcher, err := exclude.New(filepath.Join(root, "excluded") + "/**")
	if err != nil {
		t.Fatal(err)
	}

	detector := new(Watcher)
	detector.path = root
	detector.matcher = matcher
	detector.queue = watcher.NewQueue(1)
	detector.refreshPaths = make(chan string, 1)
	detector.refreshAll = make(chan struct{}, 1)

	for _, path := range []string{filepath.Join(filepath.Dir(root), "outside"), filepath.Join(root, "excluded")} {
		detector.updateDirectoryCoverage(path, "", unix.FAN_ONDIR|unix.FAN_CREATE, false, false)
	}

	if len(detector.refreshPaths) != 0 || detector.coverageLost.Load() || detector.Dirty() {
		t.Fatal("unselected directory was queued or reported as lost coverage")
	}

	selected := filepath.Join(root, "selected")
	detector.updateDirectoryCoverage(selected, "", unix.FAN_ONDIR|unix.FAN_CREATE, false, false)

	select {
	case path := <-detector.refreshPaths:
		if path != selected {
			t.Fatal("wrong directory queued", path)
		}
	default:
		t.Fatal("selected directory did not request marks")
	}
}

func TestConsumedLossDuringRefreshSchedulesImmediateRetry(t *testing.T) {
	t.Parallel()

	detector := new(Watcher)
	detector.fd = -1
	// An absent source lets the follow-up walk finish without privileged kernel marks.
	detector.path = filepath.Join(t.TempDir(), "absent")
	detector.queue = watcher.NewQueue(1)
	detector.refreshAll = make(chan struct{}, 1)
	detector.loss("simulated kernel overflow")
	<-detector.refreshAll

	// The refresher took its snapshot before the worker consumed the same loss.
	epoch := detector.coverageEpoch
	if !detector.Dirty() || detector.coverageEpoch == epoch {
		t.Fatal("consuming the loss did not invalidate the active refresh")
	}

	detector.finishCoverageRefresh(epoch, nil)

	if !detector.coverageLost.Load() || detector.Dirty() {
		t.Fatal("stale refresh restored coverage or reported another loss")
	}

	select {
	case <-detector.refreshAll:
		detector.refreshCoverage()
	default:
		t.Fatal("stale refresh deferred recovery until the periodic ticker")
	}

	if detector.coverageLost.Load() || len(detector.refreshAll) != 0 {
		t.Fatal("follow-up refresh did not restore coverage")
	}
}

func TestQueueOverflowInvalidatesCoverageOnConsumption(t *testing.T) {
	t.Parallel()

	detector := new(Watcher)
	detector.queue = watcher.NewQueue(1)
	detector.refreshAll = make(chan struct{}, 1)
	detector.queue.Send(event.Raw{Path: "/first"})
	detector.queue.Send(event.Raw{Path: "/dropped"})

	if detector.coverageLost.Load() || !detector.Dirty() || !detector.coverageLost.Load() ||
		len(detector.refreshAll) != 1 || detector.Dirty() {
		t.Fatal("queue overflow did not invalidate coverage and schedule exactly one refresh")
	}
}

func TestFailedCoverageRefreshWaitsForPeriodicRetry(t *testing.T) {
	t.Parallel()

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector := new(Watcher)
	detector.fd = -1 // FanotifyMark fails deterministically without requiring privileges.
	detector.path = t.TempDir()
	detector.matcher = matcher
	detector.queue = watcher.NewQueue(1)
	detector.refreshAll = make(chan struct{}, 1)
	detector.loss("simulated kernel overflow")
	<-detector.refreshAll
	detector.refreshCoverage()

	if !detector.coverageLost.Load() || !detector.Dirty() || len(detector.refreshAll) != 0 {
		t.Fatal("failed refresh hid coverage loss or scheduled a busy retry loop")
	}
}

// A directory that leaves the selection (to an unmarked parent, an excluded name or, without FAN_RENAME,
// anywhere) takes only its own marks along: every directory still selected keeps its mark, so no event of the
// selection was lost. Its descendants' records still need a routine reconciliation, as after an unlink.
func TestDirectoryLeavingSelectionRequestsReconciliationWithoutLoss(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	matcher, err := exclude.New(filepath.Join(root, "cache") + "/**")
	if err != nil {
		t.Fatal(err)
	}

	moved, excluded := filepath.Join(root, "plugins", "moved"), filepath.Join(root, "cache", "moved")

	for _, test := range []struct {
		name, path, old string
		mask            uint64
		hasOld, hasNew  bool
	}{
		{"one-sided rename", moved, "", unix.FAN_ONDIR | unix.FAN_RENAME, true, false},
		{"rename into an excluded name", excluded, moved, unix.FAN_ONDIR | unix.FAN_RENAME, true, true},
		{
			"rename outside root", filepath.Join(filepath.Dir(root), "outside"), moved,
			unix.FAN_ONDIR | unix.FAN_RENAME, true, true,
		},
		{"moved from without FAN_RENAME", moved, "", unix.FAN_ONDIR | unix.FAN_MOVED_FROM, false, false},
	} {
		detector := new(Watcher)
		detector.path = root
		detector.matcher = matcher
		detector.queue = watcher.NewQueue(1)
		detector.refreshPaths = make(chan string, 1)
		detector.refreshAll = make(chan struct{}, 1)

		detector.updateDirectoryCoverage(test.path, test.old, test.mask, test.hasOld, test.hasNew)

		if detector.coverageLost.Load() || detector.Dirty() || len(detector.refreshAll) != 0 {
			t.Fatal(test.name, "reported fanotify coverage loss for the selection it did not leave")
		}

		if !detector.ReconciliationPending() {
			t.Fatal(test.name, "left the departed subtree's records unreconciled")
		}
	}
}

func TestDetachedHardlinkRequestsReconciliation(t *testing.T) {
	t.Parallel()

	file, err := os.CreateTemp(t.TempDir(), "linked")
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(file)

	err = os.Link(file.Name(), file.Name()+".survivor")
	if err != nil {
		t.Fatal(err)
	}

	err = os.Remove(file.Name())
	if err != nil {
		t.Fatal(err)
	}

	detector := new(Watcher)

	detector.queue = watcher.NewQueue(1)

	err = detector.handleDescriptor(int(file.Fd()), 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	if !detector.ReconciliationPending() || len(detector.queue.Channel) != 0 || detector.Dirty() {
		t.Fatal("detached pathname became a literal event")
	}
}

func TestRealDeletedSuffixIsNotDiscarded(t *testing.T) {
	t.Parallel()

	file, err := os.Create(filepath.Join(t.TempDir(), "real (deleted)"))
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Close(file)

	matcher, err := exclude.New()
	if err != nil {
		t.Fatal(err)
	}

	detector := new(Watcher)

	detector.queue, detector.matcher = watcher.NewQueue(1), matcher

	err = detector.handleDescriptor(int(file.Fd()), 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	raw := <-detector.queue.Channel
	if raw.Dirty || raw.Path != file.Name() {
		t.Fatal("real pathname discarded", raw)
	}
}

func TestHealthyNameCoverageAvoidsFullPeriodicWalk(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	var stat unix.Stat_t

	err := unix.Lstat(root, &stat)
	if err != nil {
		t.Fatal(err)
	}

	detector := new(Watcher)
	detector.mode = ModeDFIDName
	detector.path = root
	detector.queue = watcher.NewQueue(1)
	detector.marks.Store(root, directoryIdentity{device: stat.Dev, inode: stat.Ino})

	now := time.Now()
	if detector.needsCoverageRefresh(now, now.Add(-coverageRefreshInterval)) {
		t.Fatal("healthy coverage requested full walk")
	}

	detector.coverageLost.Store(true)

	if !detector.needsCoverageRefresh(now, now) {
		t.Fatal("lost coverage was not repaired")
	}

	detector.coverageLost.Store(false)

	if !detector.needsCoverageRefresh(now, now.Add(-coverageSweepInterval)) {
		t.Fatal("bounded cleanup sweep skipped")
	}
}
