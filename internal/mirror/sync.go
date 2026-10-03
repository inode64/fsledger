// Package mirror safely synchronizes selected source paths into a Git worktree.
package mirror

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/inode64/fsledger/internal/pathindex"
	"github.com/inode64/fsledger/internal/pathutil"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/resource"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/config"
	"github.com/inode64/fsledger/internal/exclude"
)

const symlinkBufferSize = 65536

// Sync owns a repository worktree. Its caller serializes all methods. Sources are resolved roots
// whose parents contain no symlinks; SetSources replaces them when the selection changes.
type Sync struct {
	copyContent func(context.Context, io.Writer, io.Reader) error
	algorithm   string
	hashes      map[string]copiedFile
	root        *os.Root
	exclude     *exclude.Matcher
	cache       pathindex.Index[cachedFile]
	// directories lists mirror directories created or verified by the running walk; nil outside one.
	directories *pathindex.Index[struct{}]
	roots       integrity.RootGuard
	sources     []string
	// compareBuffer is reused by serialized comparisons and grows to at most two chunks.
	compareBuffer []byte
}
type signature struct {
	size   int64
	mtime  int64
	ctime  int64
	inode  uint64
	device uint64
	mode   fs.FileMode
}

// Relative maps an absolute source path into the mirror without traversal.
func Relative(path string) (string, error) {
	err := config.CleanAbsolute(path)
	if err != nil {
		return "", fault.Wrap("mirror operation", err)
	}

	relative := strings.TrimPrefix(path, "/")
	if relative == "" {
		return "", fault.New("cannot mirror filesystem root")
	}

	if pathutil.HasGitComponent(path) {
		return "", fault.New("reserved Git path: " + path)
	}

	return relative, nil
}

// Open opens an existing repository worktree with traversal-resistant operations.
func Open(path string, sources []string, matcher *exclude.Matcher, algorithm string) (*Sync, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("open mirror: %w", err)
	}

	return &Sync{
		copyContent: copyContent,
		algorithm:   algorithm, hashes: make(map[string]copiedFile),
		root:    root,
		sources: sources,
		exclude: matcher,
	}, nil
}

// SetSources replaces the mirrored roots and returns the dropped ones; the next reconciliation prunes
// whatever is no longer covered.
// Callers serialize it with every other mirror operation.
func (s *Sync) SetSources(sources []string) []string {
	var dropped []string

	for _, source := range s.sources {
		if !slices.Contains(sources, source) {
			dropped = append(dropped, source)
			s.cache.DeleteSubtree(source)
		}
	}

	s.roots.Forget(dropped)
	s.sources = sources

	return dropped
}

// RebindSources releases identities and copy signatures after a source known to
// be unavailable returns. A remount may legitimately have a new mount ID.
func (s *Sync) RebindSources(sources []string) {
	s.roots.Forget(sources)

	for _, source := range sources {
		s.cache.DeleteSubtree(source)
	}
}

// Close releases the mirror directory descriptor.
func (s *Sync) Close() error { return fault.Wrap("close mirror", s.root.Close()) }

// RefreshContext forces a copy within the caller's operation deadline.
func (s *Sync) RefreshContext(ctx context.Context, path string) error {
	err := s.roots.Check(s.sources)
	if err != nil {
		return err
	}

	return s.apply(ctx, path, copyForced)
}

// ReconcileContext checks cancellation while walking and before pruning.
func (s *Sync) ReconcileContext(ctx context.Context) error {
	clear(s.hashes)

	return s.reconcile(ctx, s.sources, copyCached)
}

func (s *Sync) apply(ctx context.Context, path string, mode copyMode) error {
	err := ctx.Err()
	if err != nil {
		return fault.Wrap("mirror cancelled", err)
	}

	if !pathutil.Within(s.sources, path) {
		return fault.New("path outside configured sources: " + path)
	}

	if s.exclude.Match(path) {
		return nil
	}

	directory, err := sourceDirectory(path)
	if integrity.Missing(err) {
		return s.remove(path)
	}

	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}

	if directory {
		return s.reconcile(ctx, []string{path}, mode)
	}

	relative, err := Relative(path)
	if err != nil {
		return err
	}

	err = s.copyStable(ctx, path, relative, mode)
	if integrity.Missing(err) {
		// The source may disappear after sourceDirectory or between copy retries.
		// Confirm its absence: ENOENT from the mirror itself is still an error.
		_, sourceErr := sourceDirectory(path)
		if integrity.Missing(sourceErr) {
			return s.remove(path)
		}
	}

	return err
}

// sourceDirectory never resolves a symlinked parent: os.Lstat would find the name
// through the link and mirror it under a path that no longer exists.
func sourceDirectory(path string) (bool, error) {
	parent, name, err := integrity.OpenParent(path)
	if err != nil {
		return false, err
	}
	defer resource.FD(parent)

	var stat unix.Stat_t

	err = unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW)

	return stat.Mode&unix.S_IFMT == unix.S_IFDIR, fault.Wrap("stat source", err)
}

// parents makes every mirror ancestor of relative a real directory. A walk visits each directory
// before its entries, so ancestors it already handled in this pass need no further syscalls.
func (s *Sync) parents(relative string) error {
	parent := filepath.Dir(relative)
	if parent == "." {
		return nil
	}

	current := ""
	for part := range strings.SplitSeq(parent, "/") {
		current = filepath.Join(current, part)

		if s.directories != nil {
			if _, known := s.directories.Get("/" + current); known {
				continue
			}
		}

		err := s.copyDirectory(current)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Sync) remove(path string) error {
	// A disappearance during copying must not prune a source lost with its filesystem.
	err := s.roots.Check(s.sources)
	if err != nil {
		return err
	}

	relative, err := Relative(path)
	if err != nil {
		return err
	}

	s.cache.DeleteSubtree(path)

	present, err := s.realParents(relative)
	if err != nil || !present {
		return err
	}

	return s.removeEntry(relative)
}

// removeEntry deletes a mirror entry and forgets any directory the running walk knew below it.
func (s *Sync) removeEntry(relative string) error {
	if s.directories != nil {
		s.directories.DeleteSubtree("/" + relative)
	}

	return fault.Wrap("remove mirror entry", s.root.RemoveAll(relative))
}

// realParents guards removal: os.Root resolves in-root symlinks, so a mirrored
// parent link would redirect RemoveAll into the directory it points to.
func (s *Sync) realParents(relative string) (bool, error) {
	current := ""

	parent := filepath.Dir(relative)
	if parent == "." {
		return true, nil
	}

	for part := range strings.SplitSeq(parent, "/") {
		current = filepath.Join(current, part)

		info, err := s.root.Lstat(current)
		if integrity.Missing(err) {
			return false, nil
		}

		if err != nil {
			return false, fault.Wrap("inspect mirror parent", err)
		}

		if !info.IsDir() {
			return false, nil
		}
	}

	return true, nil
}

// copy mirrors one source under its already derived mirror name; retries reuse that name.
func (s *Sync) copy(ctx context.Context, path, relative string, mode copyMode) error {
	err := s.parents(relative)
	if err != nil {
		return fmt.Errorf("mirror parents: %w", err)
	}

	parent, name, err := integrity.OpenParent(path)
	if err != nil {
		return fmt.Errorf("open source parent %s: %w", path, err)
	}
	defer resource.FD(parent)

	var stat unix.Stat_t

	err = unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return fmt.Errorf("source stat: %w", err)
	}

	switch stat.Mode & unix.S_IFMT {
	case unix.S_IFLNK:
		return s.copyLink(parent, name, path, relative)
	case unix.S_IFDIR:
		return s.copyDirectory(relative)
	case unix.S_IFREG:
		return s.copyRegular(ctx, parent, name, path, relative, stat, mode)
	default:
		return s.remove(path)
	}
}

func (s *Sync) copyLink(parent int, name, path, relative string) error {
	var err error

	buffer := make([]byte, symlinkBufferSize)

	n, readErr := unix.Readlinkat(parent, name, buffer)
	if readErr != nil {
		return fault.Wrap("mirror operation", readErr)
	}

	if n == len(buffer) {
		return fault.New("symlink target too long: " + path)
	}

	target := string(buffer[:n])

	existing, readErr := s.root.Readlink(relative)
	if readErr == nil && existing == target {
		return nil
	}

	err = s.removeEntry(relative)
	if err != nil {
		return err
	}

	return fault.Wrap("mirror symlink", s.root.Symlink(target, relative))
}

func (s *Sync) copyDirectory(relative string) error {
	var err error

	info, statErr := s.root.Lstat(relative)
	if statErr == nil && !info.IsDir() {
		err = s.removeEntry(relative)
		if err != nil {
			return err
		}
	}

	if statErr != nil || !info.IsDir() {
		err = s.root.Mkdir(relative, 0o700)
		if err != nil && !errors.Is(err, os.ErrExist) {
			return fault.Wrap("mirror operation", err)
		}
	}

	if s.directories != nil {
		s.directories.Set("/"+relative, struct{}{})
	}

	return nil
}

func (s *Sync) copyRegular(
	ctx context.Context,
	parent int,
	name, path, relative string,
	stat unix.Stat_t,
	mode copyMode,
) error {
	// Taken after the caller's stat; racyWindow dwarfs the gap.
	verified := time.Now().UnixNano()
	sig := sourceSignature(stat)

	if mode == copyCached {
		if previous, cached := s.unchanged(path, relative, sig); cached && !previous.racy() {
			return nil
		}
	}

	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open source %s: %w", path, err)
	}

	source := os.NewFile(uintptr(fd), path)
	defer resource.Close(source)

	err = unchangedSource(source, sig, "while opening")
	if err != nil {
		return err
	}

	// A forced copy never trusts the mirror; any other one first checks whether its bytes already match.
	if mode != copyForced {
		kept, keepErr := s.keepIdentical(ctx, source, parent, name, path, relative, sig, verified)
		if keepErr != nil || kept {
			return keepErr
		}
	}

	err = s.writeTemporary(ctx, source, parent, name, path, relative, sig)
	if err != nil {
		return err
	}

	return s.remember(path, relative, sig, verified)
}

// mirrorMode keeps mirror copies private and preserves only whether the source is executable.
func mirrorMode(source fs.FileMode) fs.FileMode {
	if source&0o111 != 0 {
		return 0o700
	}

	return 0o600
}

// unchangedSource fails with ErrUnstable once the open source no longer has the signature it was opened with.
func unchangedSource(source *os.File, expected signature, when string) error {
	info, err := source.Stat()
	if err != nil {
		return fault.Wrap("stat source", err)
	}

	current, err := metadataSignature(info)
	if err != nil {
		return err
	}

	if current != expected {
		return fmt.Errorf("%w: source changed %s: %s", integrity.ErrUnstable, when, source.Name())
	}

	return nil
}

// keepIdentical leaves the mirror copy in place when its bytes already equal the source, as for every
// unchanged file after a restart empties the cache: comparing is cheaper than a synced rewrite.
func (s *Sync) keepIdentical(
	ctx context.Context,
	source *os.File,
	parent int,
	name, path, relative string,
	sig signature,
	verified int64,
) (bool, error) {
	equal, digest, err := s.sameContents(ctx, source, relative, sig)
	if err != nil {
		return false, err
	}

	err = unchangedSource(source, sig, "during comparison")
	if err != nil {
		return false, err
	}

	if !equal {
		_, err = source.Seek(0, io.SeekStart)

		return false, fault.Wrap("rewind changed source", err)
	}

	err = verifySourceName(parent, name, path, sig)
	if err != nil {
		return false, err
	}

	s.rememberCopiedHash(path, digest, sig)

	return true, s.remember(path, relative, sig, verified)
}

func (s *Sync) remember(path, relative string, sig signature, verified int64) error {
	destination, err := s.destinationSignature(relative)
	if err != nil {
		return err
	}

	s.cache.Set(path, cachedFile{source: sig, destination: destination, verified: verified})

	return nil
}

func (s *Sync) reconcile(ctx context.Context, roots []string, mode copyMode) error {
	err := ctx.Err()
	if err != nil {
		return fault.Wrap("mirror cancelled", err)
	}

	err = s.roots.Check(s.sources)
	if err != nil {
		return err
	}

	seen := make(map[string]bool)

	s.directories = &pathindex.Index[struct{}]{}
	defer func() { s.directories = nil }()

	var failure error

	for _, source := range roots {
		walkErr := s.walkSource(ctx, source, mode, seen)
		failure = errors.Join(failure, walkErr)
	}

	unstable, fatal := integrity.SplitUnstable(failure)

	err = errors.Join(ctx.Err(), s.roots.Check(s.sources))
	if err != nil || fatal != nil {
		return errors.Join(err, failure)
	}

	return errors.Join(failure, s.prune(ctx, roots, seen, unstable))
}

func (s *Sync) walkSource(ctx context.Context, source string, mode copyMode, seen map[string]bool) error {
	var failure error

	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		err := ctx.Err()
		if err != nil {
			return fault.Wrap("walk cancelled", err)
		}

		if integrity.Missing(walkErr) {
			return nil
		}

		if walkErr != nil {
			return walkErr
		}

		if s.walkExcluded(path, source) {
			if entry.IsDir() {
				return filepath.SkipDir
			}

			return nil
		}

		copyErr := s.copySeen(ctx, path, mode, seen)
		if errors.Is(copyErr, integrity.ErrUnstable) {
			failure = errors.Join(failure, copyErr)

			return nil
		}

		return copyErr
	})

	return fault.Wrap("walk source "+source, errors.Join(err, failure))
}

func (s *Sync) prune(ctx context.Context, roots []string, seen map[string]bool, unstable []string) error {
	full := slices.Equal(roots, s.sources)

	var stale []string

	err := fs.WalkDir(s.root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		cancelErr := ctx.Err()
		if cancelErr != nil {
			return fault.Wrap("prune cancelled", cancelErr)
		}

		if walkErr != nil {
			return walkErr
		}

		if path == "." {
			return nil
		}

		protected := pathutil.Within(unstable, "/"+path)
		if skipPrune(path, entry, protected, !full && !s.overlapsRoots("/"+path, roots)) {
			return skipDirectory(entry)
		}

		if protected || !staleEntry(path, roots, seen, full) {
			return nil
		}

		stale = append(stale, path)

		if entry.IsDir() {
			return filepath.SkipDir
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("walk mirror: %w", err)
	}

	return s.removeStale(ctx, stale)
}

func skipDirectory(entry fs.DirEntry) error {
	if entry.IsDir() {
		return filepath.SkipDir
	}

	return nil
}

func skipPrune(path string, entry fs.DirEntry, protected, outside bool) bool {
	return strings.EqualFold(path, ".git") || (entry.IsDir() && (protected || outside))
}

func (s *Sync) removeStale(ctx context.Context, stale []string) error {
	for _, path := range stale {
		err := ctx.Err()
		if err != nil {
			return fault.Wrap("prune cancelled", err)
		}

		err = s.removeEntry(path)
		if err != nil {
			return err
		}

		s.cache.DeleteSubtree("/" + path)
	}

	return nil
}

func staleEntry(path string, roots []string, seen map[string]bool, full bool) bool {
	absolute := "/" + path
	inScope := false

	for _, source := range roots {
		if pathutil.Contains(absolute, source) {
			return false
		}

		if pathutil.Contains(source, absolute) {
			inScope = true
		}
	}

	// Seen entries passed the exclusions while walking the sources.
	return (inScope || full) && !seen[path]
}

// unchanged returns the cached observation when neither the source nor its mirror copy moved since.
// A forced copy never trusts the cache, and so never inspects the mirror copy.
func (s *Sync) unchanged(path, relative string, sig signature) (cachedFile, bool) {
	previous, ok := s.cache.Get(path)
	if !ok || previous.source != sig {
		return previous, false
	}

	destination, err := s.destinationSignature(relative)

	return previous, err == nil && destination == previous.destination
}

func (s *Sync) writeTemporary(
	ctx context.Context,
	source *os.File,
	parent int,
	name, path, relative string,
	sig signature,
) error {
	// Replace through a private temporary sibling; never follow a mirror symlink.
	temp, err := os.CreateTemp(s.root.Name(), ".fsledger-copy-*")
	if err != nil {
		return fault.Wrap("mirror operation", err)
	}

	tempName := filepath.Base(temp.Name())
	defer resource.Remove(s.root, tempName)

	digest, err := s.copyToTemporary(ctx, temp, source, sig)
	if err != nil {
		return err
	}

	err = ctx.Err()
	if err != nil {
		return fault.Wrap("copy cancelled", err)
	}

	err = unchangedSource(source, sig, "while copying")
	if err != nil {
		return err
	}

	err = verifySourceName(parent, name, path, sig)
	if err != nil {
		return err
	}

	info, statErr := s.root.Lstat(relative)
	if statErr == nil && info.IsDir() {
		err = s.removeEntry(relative)
		if err != nil {
			return err
		}
	}

	err = s.root.Rename(tempName, relative)
	if err != nil {
		return fmt.Errorf("replace mirror: %w", err)
	}

	s.rememberCopiedHash(path, digest, sig)

	return nil
}

func (s *Sync) rememberCopiedHash(path string, digest hash.Hash, copied signature) {
	if digest == nil {
		return
	}

	s.hashes[path] = copiedFile{
		signature: copied,
		digest:    hex.EncodeToString(digest.Sum(nil)),
		hashedAt:  time.Now().UnixNano(),
	}
}

func verifySourceName(parent int, name, path string, expected signature) error {
	var stat unix.Stat_t

	err := unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		if integrity.Missing(err) {
			return fmt.Errorf("%w: source name changed after opening: %s", integrity.ErrUnstable, path)
		}

		return fmt.Errorf("verify source name %s: %w", path, err)
	}

	if sourceSignature(stat) != expected {
		return fmt.Errorf("%w: source name changed after opening: %s", integrity.ErrUnstable, path)
	}

	return nil
}

func (s *Sync) copySeen(ctx context.Context, path string, mode copyMode, seen map[string]bool) error {
	relative, err := Relative(path)
	if err != nil {
		return err
	}

	seen[relative] = true

	err = s.copyStable(ctx, path, relative, mode)
	if integrity.Missing(err) {
		delete(seen, relative)

		return nil
	}

	return err
}

// WalkDir never enters an excluded directory, so only the root needs its ancestors checked.
func (s *Sync) walkExcluded(path, source string) bool {
	if path == source {
		return s.exclude.Match(path)
	}

	return s.exclude.MatchEntry(path)
}

func sourceSignature(stat unix.Stat_t) signature {
	return signature{
		size:   stat.Size,
		mtime:  stat.Mtim.Nano(),
		ctime:  stat.Ctim.Nano(),
		inode:  stat.Ino,
		device: stat.Dev,
		mode:   fs.FileMode(stat.Mode),
	}
}

func (s *Sync) copyToTemporary(ctx context.Context, temp, source *os.File, sig signature) (hash.Hash, error) {
	digest, err := s.newDigest()
	if err != nil {
		resource.Close(temp)

		return nil, err
	}

	var output io.Writer = temp
	if digest != nil {
		output = io.MultiWriter(temp, digest)
	}

	copyErr := s.copyContent(ctx, output, io.LimitReader(source, sig.size))
	if copyErr == nil {
		info, statErr := temp.Stat()

		copyErr = statErr
		if statErr == nil {
			copyErr = verifyCopiedContent(source, sig, info.Size())
		}
	}

	modeErr := temp.Chmod(mirrorMode(sig.mode))
	syncErr := temp.Sync()

	closeErr := temp.Close()

	err = errors.Join(copyErr, modeErr, syncErr, closeErr)
	if err != nil {
		return nil, fmt.Errorf("copy source: %w", err)
	}

	return digest, nil
}

func (*Sync) overlapsRoots(path string, roots []string) bool {
	for _, root := range roots {
		if pathutil.Contains(root, path) || pathutil.Contains(path, root) {
			return true
		}
	}

	return false
}
