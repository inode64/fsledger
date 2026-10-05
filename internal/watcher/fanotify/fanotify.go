// Package fanotify implements Linux notification groups directly with x/sys/unix.
// Basic FD events provide attribution; inotify supplies namespace changes in V1.
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

// Watcher owns a basic-FD fanotify group with optional PIDFD records.
type Watcher struct {
	cancel        context.CancelFunc
	coverageDone  chan struct{}
	refreshPaths  chan string
	refreshAll    chan struct{}
	done          chan struct{}
	handles       *filehandle.Resolver
	matcher       *exclude.Matcher
	queue         *watcher.Queue
	marks         sync.Map
	mode          string
	path          string
	coverageEpoch uint64
	mask          uint64
	fd            int
	once          sync.Once
	coverageMutex sync.Mutex
	coverageLost  atomic.Bool
	pidfd         bool
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

func signed(value []byte) int {
	//nolint:gosec // fanotify's ABI encodes signed 32-bit fd/PID values, including -1.
	return int(int32(binary.NativeEndian.Uint32(value)))
}

func readPIDFD(info []byte) (*os.File, error) {
	var result *os.File

	for len(info) > 0 {
		if len(info) < infoHeaderSize {
			resource.OptionalFile(result)

			return nil, fault.New("truncated fanotify info header")
		}

		length := int(binary.NativeEndian.Uint16(info[2:4]))
		if length < infoHeaderSize || length > len(info) {
			resource.OptionalFile(result)

			return nil, fault.New("invalid fanotify info length")
		}

		if info[0] == unix.FAN_EVENT_INFO_TYPE_PIDFD && length >= 8 {
			descriptor := signed(info[4:8])
			if descriptor >= 0 {
				resource.OptionalFile(result)

				result = os.NewFile(uintptr(descriptor), "fanotify-pidfd")
			}
		}

		info = info[length:]
	}

	return result, nil
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

	err = filepath.WalkDir(path, func(name string, entry fs.DirEntry, walkErr error) error {
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

		return w.markExisting(name, verified)
	})

	return fault.Wrap("install fanotify marks", err)
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

	w.marks.Range(func(key, value any) bool {
		path, valid := key.(string)

		identity, known := value.(directoryIdentity)
		if !valid || !known || (!verified[path] && !matchesDirectory(path, identity)) {
			w.marks.Delete(key)
		}

		return true
	})
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

// requestTree queues a path already selected by updateDirectoryCoverage.
func (w *Watcher) requestTree(path string) {
	select {
	case w.refreshPaths <- path:
	default:
		w.loss("fanotify coverage refresh queue overflow")
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

	pidfd, err := readPIDFD(record[metadata:])
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
		return w.handleFID(record[metadata:], pid, pidfd, mask)
	}

	if descriptor < 0 {
		resource.OptionalFile(pidfd)

		w.loss("fanotify event without file descriptor")

		return nil
	}

	return w.handleDescriptor(descriptor, pid, pidfd)
}

func (w *Watcher) handleDescriptor(descriptor, pid int, pidfd *os.File) error {
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

	if w.matcher.Match(path) {
		resource.OptionalFile(pidfd)

		return nil
	}

	w.queue.Send(
		event.Raw{Path: path, PID: pid, PIDFD: pidfd, Backend: w.Name(), Time: time.Now(), Operation: event.Write},
	)

	return nil
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
