package integrity

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/inode64/fsledger/internal/pathutil"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/exclude"
	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/resource"
)

const directoryBatchSize = 128

// Scanner shares a global concurrency budget across every repository.
type Scanner struct {
	slots chan *readWorkspace
	scans chan struct{}
	roots RootGuard
}

// NewScanner constructs a bounded pool. Both limits must be positive, as configuration validation ensures.
func NewScanner(workers, concurrent int) *Scanner {
	scanner := &Scanner{
		slots: make(chan *readWorkspace, workers),
		scans: make(chan struct{}, concurrent),
		roots: RootGuard{},
	}
	for range cap(scanner.slots) {
		scanner.slots <- &readWorkspace{}
	}

	return scanner
}

// Observe shares the same global budget with full scans.
func (scanner *Scanner) Observe(
	ctx context.Context,
	path, algorithm string,
	content bool,
	copied ...HashSource,
) (Record, error) {
	var workspace *readWorkspace

	select {
	case workspace = <-scanner.slots:
	case <-ctx.Done():
		return Record{}, fault.Wrap("observation cancelled", ctx.Err())
	}

	defer func() { scanner.slots <- workspace }()

	const attempts = 3

	var (
		record Record
		err    error
	)
	for range attempts {
		record, err = workspace.read(ctx, path, algorithm, content, firstHashSource(copied))
		if !errors.Is(err, ErrUnstable) {
			break
		}
	}

	if errors.Is(err, ErrUnstable) {
		err = UnstablePathsError{path}
	}

	return record, err
}

type observation struct {
	err    error
	record Record
}

// ScanPaths walks only the requested paths while guarding configured roots, not
// temporary subdirectories which may legitimately disappear between events.
func (scanner *Scanner) ScanPaths(
	ctx context.Context,
	roots, paths []string,
	matcher *exclude.Matcher,
	algorithm string,
	content bool,
	visit func(Record) error,
	copied ...HashSource,
) error {
	admissionErr := scanner.admitScan(ctx)
	if admissionErr != nil {
		return admissionErr
	}

	defer func() { <-scanner.scans }()

	if len(paths) == 0 {
		paths = roots
	}

	err := scanner.checkPaths(roots, paths)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan string, cap(scanner.slots))
	results := make(chan observation, cap(scanner.slots))
	walkDone := make(chan error, 1)

	go func() {
		defer close(jobs)

		walkDone <- scanner.walkPaths(ctx, paths, matcher, jobs)
	}()

	var workers sync.WaitGroup
	for range cap(scanner.slots) {
		workers.Go(func() {
			for path := range jobs {
				record, err := scanner.Observe(ctx, path, algorithm, content, copied...)
				select {
				case results <- observation{record: record, err: err}:
				case <-ctx.Done():
					return
				}
			}
		})
	}

	go func() { workers.Wait(); close(results) }()

	failure := consume(results, visit, cancel)

	return errors.Join(failure, <-walkDone, ctx.Err(), scanner.CheckRoots(roots))
}

func withinRoots(roots []string, path string) bool {
	return pathutil.ValidAbsolute(path) && pathutil.Within(roots, path)
}

func consume(results <-chan observation, visit func(Record) error, cancel context.CancelFunc) error {
	var failures []error

	stopped := false
	for result := range results {
		if stopped {
			continue
		}

		fatal, err := consumeObservation(result, visit)
		if err != nil {
			failures = append(failures, err)
		}

		if fatal {
			stopped = true

			cancel()
		}
	}

	return errors.Join(failures...)
}

func consumeObservation(result observation, visit func(Record) error) (bool, error) {
	if errors.Is(result.err, os.ErrNotExist) {
		return false, nil
	}

	if result.err != nil {
		return false, result.err
	}

	err := visit(result.record)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	return err != nil && !errors.Is(err, ErrUnstable), err
}

// CheckRoots verifies source availability independently of individual file observations.
func (scanner *Scanner) CheckRoots(roots []string) error { return scanner.roots.Check(roots) }

// ForgetRoots releases sources that left the configured selection.
func (scanner *Scanner) ForgetRoots(roots []string) { scanner.roots.Forget(roots) }

// RootUnavailable reports a pinned source lost together with its filesystem rather than deleted.
func (scanner *Scanner) RootUnavailable(root string) bool { return scanner.roots.Unavailable(root) }

// Do shares the scan budget with the existing serial Git mirror implementation.
func (scanner *Scanner) Do(ctx context.Context, operation func() error) error {
	var workspace *readWorkspace

	select {
	case workspace = <-scanner.slots:
	case <-ctx.Done():
		return fault.Wrap("scan cancelled", ctx.Err())
	}

	defer func() { scanner.slots <- workspace }()

	return operation()
}

func (scanner *Scanner) checkPaths(roots, paths []string) error {
	for _, path := range paths {
		if !withinRoots(roots, path) {
			return fault.New("scan path outside configured sources: " + path)
		}
	}

	return scanner.CheckRoots(roots)
}

// walk checks entries by name only: walkPaths checked the root's ancestors and no excluded directory is entered.
func (scanner *Scanner) walk(ctx context.Context, path string, matcher *exclude.Matcher, jobs chan<- string) error {
	if matcher.MatchEntry(path) {
		return nil
	}

	select {
	case jobs <- path:
	case <-ctx.Done():
		return fault.Wrap("scan cancelled", ctx.Err())
	}

	parent, name, err := OpenParent(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return err
	}

	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	resource.FD(parent)

	if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) || errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fault.Wrap("open scan directory", err)
	}

	directory := os.NewFile(uintptr(fd), path)
	defer resource.Close(directory)

	return scanner.walkChildren(ctx, directory, path, matcher, jobs)
}

func (scanner *Scanner) walkChildren(
	ctx context.Context,
	directory *os.File,
	path string,
	matcher *exclude.Matcher,
	jobs chan<- string,
) error {
	for {
		entries, readErr := directory.ReadDir(directoryBatchSize)
		for _, entry := range entries {
			child := filepath.Join(path, entry.Name())

			err := scanner.walkEntry(ctx, child, entry, matcher, jobs)
			if err != nil {
				return err
			}
		}

		if errors.Is(readErr, io.EOF) {
			return nil
		}

		if readErr != nil {
			return fault.Wrap("read scan directory", readErr)
		}
	}
}

func firstHashSource(sources []HashSource) HashSource {
	if len(sources) == 0 {
		return nil
	}

	return sources[0]
}

func (scanner *Scanner) walkEntry(
	ctx context.Context,
	path string,
	entry os.DirEntry,
	matcher *exclude.Matcher,
	jobs chan<- string,
) error {
	if entry.IsDir() {
		return scanner.walk(ctx, path, matcher, jobs)
	}

	if matcher.MatchEntry(path) {
		return nil
	}

	select {
	case jobs <- path:
		return nil
	case <-ctx.Done():
		return fault.Wrap("scan cancelled", ctx.Err())
	}
}

// Admission is independent of file slots: prepare() may observe a file again
// from the visitor, even with one worker. Waiting must not reserve a file slot.
func (scanner *Scanner) admitScan(ctx context.Context) error {
	select {
	case scanner.scans <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fault.Wrap("scan admission cancelled", ctx.Err())
	}
}

func (scanner *Scanner) walkPaths(
	ctx context.Context,
	paths []string,
	matcher *exclude.Matcher,
	jobs chan<- string,
) error {
	for _, root := range paths {
		if matcher.Match(root) {
			continue
		}

		err := scanner.walk(ctx, root, matcher, jobs)
		if err != nil {
			return err
		}
	}

	return nil
}
