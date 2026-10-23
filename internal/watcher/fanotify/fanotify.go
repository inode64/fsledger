// Package fanotify implements Linux notification groups directly with x/sys/unix.
// Every mode attributes writes; the fid and dfid_name modes also report creations, deletions and renames.
package fanotify

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/filehandle"
	"github.com/inode64/fsledger/internal/pathindex"
	"github.com/inode64/fsledger/internal/pathutil"
	"github.com/inode64/fsledger/internal/resource"
	"github.com/inode64/fsledger/internal/watcher"
)

const (
	metadataSize      = 24
	coverageQueueSize = 64
	infoHeaderSize    = 4
	eventBufferSize   = 64 * 1024
	pollMilliseconds  = 100
	// FAN_ATTRIB requires FID groups; metadata changes use the companion inotify/polling backend.
	notificationMask = unix.FAN_CLOSE_WRITE | unix.FAN_MODIFY | unix.FAN_EVENT_ON_CHILD
)

// Watcher owns one fanotify group in fd, fid or dfid_name mode, with optional PIDFD records.
type Watcher struct {
	cancel       context.CancelFunc
	coverageDone chan struct{}
	refreshPaths chan string
	refreshAll   chan struct{}
	done         chan struct{}
	handles      *filehandle.Resolver
	matcher      *exclude.Matcher
	queue        *watcher.Queue
	// marks is shared by the reader, coverage refreshes and Covers.
	marks         pathindex.Index[directoryIdentity]
	mode          string
	path          string
	marksMutex    sync.Mutex
	coverageEpoch uint64
	mask          uint64
	// device is the root's filesystem, the one the handle anchor resolves; marks never leave it.
	device        uint64
	fd            int
	once          sync.Once
	coverageMutex sync.Mutex
	coverageLost  atomic.Bool
	// unmarked reports directories left unmarked below the root, on another filesystem or beyond the mark
	// limit; only a walk of the root clears it.
	unmarked atomic.Bool
	// markLimit reports that the last walk of the root hit the kernel's mark limit.
	markLimit atomic.Bool
	pidfd     bool
}

// New tries PIDFD independently and falls back to ordinary PID events.
func New(path string, matcher *exclude.Matcher) (*Watcher, error) {
	detector := &Watcher{
		refreshPaths: make(
			chan string,
			coverageQueueSize,
		),
		refreshAll:   make(chan struct{}, 1),
		fd:           -1,
		path:         path,
		matcher:      matcher,
		queue:        watcher.NewQueue(watcher.QueueSize),
		done:         make(chan struct{}),
		coverageDone: make(chan struct{}),
	}

	err := detector.initialize()
	if err != nil {
		return nil, err
	}

	return detector, nil
}

// Name identifies this detector's notifications.
func (*Watcher) Name() string { return event.Fanotify }

// LossReason consumes the cause of the loss reported by Dirty.
func (w *Watcher) LossReason() string { return w.queue.LossReason() }

// Events exposes a nonblocking bounded stream.
func (w *Watcher) Events() <-chan *event.Raw { return w.queue.Channel }

// Dirty consumes dropped events and kernel overflow state.
func (w *Watcher) Dirty() bool {
	dirty := w.queue.Dirty()
	if dirty {
		w.invalidateCoverage()
	}

	return dirty
}

// ReconciliationPending consumes routine requests independently of overflow.
func (w *Watcher) ReconciliationPending() bool { return w.queue.ReconciliationPending() }

// MarkLimitReached reports directories left unmarked by the kernel's mark limit, which reconciliation covers.
func (w *Watcher) MarkLimitReached() bool { return w.markLimit.Load() }

// Start installs recursive marks before starting the reader and coverage refresher.
func (w *Watcher) Start(ctx context.Context) error {
	err := w.markTree(w.path, nil)
	if err != nil {
		return fmt.Errorf("fanotify mark %s: %w", w.path, err)
	}

	ctx, w.cancel = context.WithCancel(ctx)
	go w.read(ctx)
	go w.refresh(ctx)

	return nil
}

// Close stops readers before releasing the group and any unconsumed PIDFDs.
func (w *Watcher) Close() error {
	var err error

	w.once.Do(func() {
		if w.cancel != nil {
			w.cancel()
			<-w.done
			<-w.coverageDone
		}

		err = unix.Close(w.fd)

		if w.handles != nil {
			err = errors.Join(err, w.handles.Close())
		}

		for {
			select {
			case raw := <-w.queue.Channel:
				resource.OptionalFile(raw.PIDFD)
			default:
				return
			}
		}
	})

	return fault.Wrap("close fanotify", err)
}

// sinceBoot reads CLOCK_BOOTTIME, the clock of /proc/<pid>/stat start times; zero means unknown.
func sinceBoot() time.Duration {
	var now unix.Timespec

	err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &now)
	if err != nil {
		return 0
	}

	return time.Duration(now.Nano())
}

func signed(value []byte) int {
	//nolint:gosec // fanotify's ABI encodes signed 32-bit fd/PID values, including -1.
	return int(int32(binary.NativeEndian.Uint32(value)))
}

// readPIDFD also reports FAN_NOPIDFD, which marks a writer already reaped when the event was read.
func readPIDFD(info []byte) (*os.File, bool, error) {
	var result *os.File

	reaped := false

	for len(info) > 0 {
		if len(info) < infoHeaderSize {
			resource.OptionalFile(result)

			return nil, false, fault.New("truncated fanotify info header")
		}

		length := int(binary.NativeEndian.Uint16(info[2:4]))
		if length < infoHeaderSize || length > len(info) {
			resource.OptionalFile(result)

			return nil, false, fault.New("invalid fanotify info length")
		}

		if info[0] == unix.FAN_EVENT_INFO_TYPE_PIDFD && length >= 8 {
			descriptor := signed(info[4:8])
			reaped = descriptor == unix.FAN_NOPIDFD

			if descriptor >= 0 {
				resource.OptionalFile(result)

				result = os.NewFile(uintptr(descriptor), "fanotify-pidfd")
			}
		}

		info = info[length:]
	}

	return result, reaped, nil
}

func (w *Watcher) markTree(path string, verified map[string]bool) error {
	path = filepath.Clean(path)
	if !pathutil.Contains(w.path, path) {
		return fault.New("refuse fanotify mark outside watched root: " + path)
	}

	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fault.Wrap("stat watched root", err)
	}

	if !info.IsDir() {
		return w.markExisting(filepath.Dir(path), verified)
	}

	if w.matcher.Match(path) {
		return nil
	}

	skipped, limited, err := w.walkTree(path, verified)
	// A subtree cannot tell that nothing else remains unmarked below the root; only a walk of the root can.
	if err == nil && (skipped || path == w.path) {
		w.unmarked.Store(skipped)
		w.markLimit.Store(limited)
	}

	return err
}

func markLimit(err error) bool { return errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.ENOMEM) }

// walkTree marks the selected directories below path and reports whether it left directories unmarked, and
// whether the mark limit was the cause.
func (w *Watcher) walkTree(path string, verified map[string]bool) (bool, bool, error) {
	skipped, limited := false, false

	err := filepath.WalkDir(path, func(name string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil
		}

		if walkErr != nil {
			return walkErr
		}

		if !entry.IsDir() {
			return nil
		}

		if w.matcher.MatchEntry(name) {
			return filepath.SkipDir
		}

		markErr := w.markExisting(name, verified)
		// A filesystem mounted below the root after Start is a boundary, like the nested mounts excluded at startup,
		// but without a watcher of its own: reconciliation covers it. So is a directory beyond the mark limit;
		// treating it as lost coverage would flush and re-mark every 30 seconds. At the root it is lost coverage.
		if name != w.path && (errors.Is(markErr, errOtherFilesystem) || markLimit(markErr)) {
			skipped, limited = true, limited || markLimit(markErr)

			return filepath.SkipDir
		}

		return markErr
	})

	return skipped, limited, fault.Wrap("install fanotify marks", err)
}

const (
	coverageRefreshInterval = 30 * time.Second
	coverageSweepInterval   = 10 * time.Minute
)

func (w *Watcher) refresh(ctx context.Context) {
	defer close(w.coverageDone)

	lastSweep := time.Now()

	ticker := time.NewTicker(coverageRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case path := <-w.refreshPaths:
			err := w.markTree(path, nil)
			if err != nil {
				w.loss(err.Error())
			}
		case <-w.refreshAll:
			w.refreshCoverage()

			lastSweep = time.Now()
		case now := <-ticker.C:
			if w.needsCoverageRefresh(now, lastSweep) {
				w.refreshCoverage()

				lastSweep = now
			}
		}
	}
}

func (w *Watcher) refreshCoverage() {
	w.coverageMutex.Lock()
	epoch := w.coverageEpoch
	w.coverageMutex.Unlock()

	if w.coverageLost.Load() {
		err := w.resetMarks()
		if err != nil {
			w.loss(err.Error())

			return
		}
	}

	verified := make(map[string]bool)

	err := w.markTree(w.path, verified)
	if err != nil {
		w.loss(err.Error())

		return
	}

	w.finishCoverageRefresh(epoch, verified)
}

func (w *Watcher) finishCoverageRefresh(epoch uint64, verified map[string]bool) {
	// A loss that arrived during this walk cannot be acknowledged by it.
	w.coverageMutex.Lock()
	defer w.coverageMutex.Unlock()

	if epoch == w.coverageEpoch {
		w.coverageLost.Store(false)
	} else {
		// The walk succeeded, but its snapshot is stale. Retry without waiting for the ticker.
		w.requestCoverageRefresh()
	}

	w.marksMutex.Lock()
	defer w.marksMutex.Unlock()

	var stale []string

	for path, identity := range w.marks.All() {
		if !verified[path] && !matchesDirectory(path, identity) {
			stale = append(stale, path)
		}
	}

	for _, path := range stale {
		w.marks.Delete(path)
	}
}

func (w *Watcher) loss(reason string) {
	w.invalidateCoverage()
	w.queue.Send(event.Raw{Dirty: true, Reason: reason, Backend: w.Name()})
}

func (w *Watcher) invalidateCoverage() {
	w.coverageMutex.Lock()
	newlyLost := !w.coverageLost.Swap(true)
	w.coverageEpoch++
	w.coverageMutex.Unlock()

	if newlyLost {
		w.requestCoverageRefresh()
	}
}

func (w *Watcher) requestCoverageRefresh() {
	select {
	case w.refreshAll <- struct{}{}:
	default:
	}
}

// requestTree queues a path already selected by updateDirectoryCoverage. A burst of new directories that
// overflows the queue loses no event: a full refresh marks them, and Covers leaves them to inotify meanwhile.
func (w *Watcher) requestTree(path string) {
	select {
	case w.refreshPaths <- path:
	default:
		w.requestCoverageRefresh()
	}
}

func (w *Watcher) transientPath() {
	w.queue.Send(event.Raw{Dirty: true, Reason: event.Reconciliation, Backend: w.Name()})
}

func (w *Watcher) read(ctx context.Context) {
	defer close(w.done)

	buffer := make([]byte, eventBufferSize)

	for ctx.Err() == nil {
		//nolint:gosec // Linux supplies bounded descriptors/UIDs; the adjacent checks or ABI define the range.
		fds := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN, Revents: 0}}

		_, err := unix.Poll(fds, pollMilliseconds)
		if errors.Is(err, unix.EINTR) {
			continue
		}

		if err != nil {
			w.loss(err.Error())

			return
		}

		if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			w.loss("fanotify reader lost coverage")

			return
		}

		if fds[0].Revents&unix.POLLIN == 0 {
			continue
		}

		readBatchErr := w.readBatch(buffer)
		if readBatchErr != nil {
			w.loss(readBatchErr.Error())
		}
	}
}

func (w *Watcher) readBatch(buffer []byte) error {
	count, err := unix.Read(w.fd, buffer)
	if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
		return nil
	}

	if err != nil {
		return fault.Wrap("read fanotify", err)
	}

	return w.decode(buffer[:count])
}

func (w *Watcher) decode(buffer []byte) error {
	var result error

	for len(buffer) > 0 {
		if len(buffer) < metadataSize {
			return fault.New("truncated fanotify metadata")
		}

		length := int(binary.NativeEndian.Uint32(buffer[:4]))

		metadata := int(binary.NativeEndian.Uint16(buffer[6:8]))
		if length < metadataSize || length > len(buffer) || metadata < metadataSize || metadata > length {
			return fault.New("invalid fanotify record length")
		}

		record := buffer[:length]
		buffer = buffer[length:]

		err := w.handle(record, metadata)
		result = errors.Join(result, err)
	}

	return result
}

func (w *Watcher) handle(record []byte, metadata int) error {
	descriptor := signed(record[16:20])
	pid := signed(record[20:24])

	if descriptor >= 0 {
		defer resource.FD(descriptor)
	}

	pidfd, reaped, err := readPIDFD(record[metadata:])
	if err != nil {
		return err
	}

	if record[4] != unix.FANOTIFY_METADATA_VERSION {
		resource.OptionalFile(pidfd)

		return fault.New("unsupported fanotify metadata version")
	}

	mask := binary.NativeEndian.Uint64(record[8:16])
	if mask&unix.FAN_Q_OVERFLOW != 0 {
		resource.OptionalFile(pidfd)

		w.loss("FAN_Q_OVERFLOW")

		return nil
	}

	if descriptor < 0 && w.handles != nil {
		return w.handleFID(record[metadata:], pid, pidfd, reaped, mask)
	}

	if descriptor < 0 {
		resource.OptionalFile(pidfd)

		w.loss("fanotify event without file descriptor")

		return nil
	}

	return w.handleDescriptor(descriptor, pid, pidfd, reaped)
}

func (w *Watcher) handleDescriptor(descriptor, pid int, pidfd *os.File, reaped bool) error {
	path, err := filehandle.DescriptorPath(descriptor)
	if errors.Is(err, filehandle.ErrDeleted) {
		resource.OptionalFile(pidfd)
		w.transientPath()

		return nil
	}

	if err != nil {
		resource.OptionalFile(pidfd)

		return err
	}

	if w.ignored(path) {
		resource.OptionalFile(pidfd)

		return nil
	}

	w.queue.Send(event.Raw{
		Path: path, PID: pid, PIDFD: pidfd, Reaped: reaped,
		Backend: w.Name(), Time: time.Now(), SinceBoot: sinceBoot(), Operation: event.Write,
	})

	return nil
}

// ignored drops events outside the root, which a file root's marked parent or a directory moved out of the
// root still reports, as well as excluded paths: they would cost procfs resolution only to be discarded.
func (w *Watcher) ignored(path string) bool {
	return !pathutil.Contains(w.path, path) || w.matcher.Match(path)
}

func (w *Watcher) markExisting(path string, verified map[string]bool) error {
	err := w.mark(path)
	if err == nil && verified != nil {
		verified[path] = true
	}

	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	return err
}
