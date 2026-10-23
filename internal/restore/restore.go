// Package restore writes a Git archive into a destination directory under an overwrite policy.
package restore

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/pathutil"
	"github.com/inode64/fsledger/internal/resource"
)

// ErrSourceMissing reports that the archive never contained the requested prefix.
var ErrSourceMissing = errors.New("archived path not found")

// Policy decides what happens when a destination entry already exists.
type Policy int

// Overwrite policies accepted by --overwrite.
const (
	Off Policy = iota
	Always
	Newer
)

// ParsePolicy maps the command line spelling to a Policy.
func ParsePolicy(value string) (Policy, error) {
	switch value {
	case "off":
		return Off, nil
	case "always":
		return Always, nil
	case "newer":
		return Newer, nil
	default:
		return Off, fault.New("--overwrite must be off, always or newer")
	}
}

// Options describe one restore run.
type Options struct {
	// Committed dates the archived commit; Newer compares it with destination modification times.
	Committed time.Time
	// Output receives one line per archived entry under Prefix.
	Output io.Writer
	// Destination is where Prefix lands: a directory when Prefix is a tree, otherwise the new path
	// of the single archived file or symlink.
	Destination string
	// Prefix is the archived path being restored; ancestors and siblings are ignored.
	Prefix string
	Policy Policy
	DryRun bool
}

// Summary counts the outcome of every archived entry under the prefix.
type Summary struct {
	Restored int
	Skipped  int
	Failed   int
}

const (
	fileMode        = 0o644
	executableMode  = 0o755
	executableBits  = 0o111
	directoryMode   = 0o755
	temporaryPrefix = pathutil.RestoreTemporaryPrefix
	// preservedBits is the part of a replaced file's mode its replacement inherits.
	preservedBits = fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky
)

type action int

const (
	actionNone action = iota
	actionRestore
	actionSkip
	actionFail
)

// run holds the destination state discovered from the archive's prefix entry.
type run struct {
	// failed holds the directory entries that failed; nothing below them is attempted.
	failed map[string]struct{}
	// base is the opened directory shown in output lines.
	base string
	// name is the destination base name when Prefix is a single file or symlink.
	name    string
	options Options
	// root is -1 before the prefix entry arrives, and stays -1 in a dry run over an absent destination.
	root   int
	opened bool
}

// Apply walks the archive and restores every entry under Options.Prefix. The prefix entry itself
// decides the destination layout, so nothing is created before it is seen.
func Apply(ctx context.Context, reader *tar.Reader, options Options) (Summary, error) {
	state := run{failed: map[string]struct{}{}, base: "", name: "", options: options, root: -1, opened: false}
	defer state.close()

	var summary Summary

	for {
		err := ctx.Err()
		if err != nil {
			return summary, fault.Wrap("restore cancelled", err)
		}

		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return summary, state.finish()
		}

		if err != nil {
			return summary, fault.Wrap("read archive", err)
		}

		err = state.step(reader, header, &summary)
		if err != nil {
			return summary, err
		}
	}
}

// finish reports an archive that ended without the prefix entry.
func (s *run) finish() error {
	if !s.opened {
		return ErrSourceMissing
	}

	return nil
}

// step handles one archive entry and records its outcome.
func (s *run) step(reader io.Reader, header *tar.Header, summary *Summary) error {
	relative, included, err := s.locate(header)
	if err != nil || !included {
		return err
	}

	outcome, reason := s.entry(reader, header, relative)
	if outcome == actionFail && header.Typeflag == tar.TypeDir {
		s.failed[relative] = struct{}{}
	}

	return s.report(summary, outcome, relative, reason)
}

func (s *run) close() {
	if s.root >= 0 {
		resource.FD(s.root)
	}
}

// locate maps an archive name to a destination path. The prefix entry opens the destination;
// ancestors, siblings and the prefix directory itself are excluded.
func (s *run) locate(header *tar.Header) (string, bool, error) {
	clean := path.Clean(strings.TrimSuffix(header.Name, "/"))

	if s.opened {
		if s.name != "" {
			return "", false, nil
		}

		relative, included := strings.CutPrefix(clean, s.options.Prefix+"/")

		return relative, included, nil
	}

	if clean != s.options.Prefix {
		return "", false, nil
	}

	s.opened = true

	if header.Typeflag == tar.TypeDir {
		return "", false, s.open(s.options.Destination, true)
	}

	s.name = filepath.Base(s.options.Destination)

	return s.name, true, s.open(filepath.Dir(s.options.Destination), false)
}

// open prepares the destination directory without following a symlink in any of its components,
// since its owner may differ from the root running the restore. A dry run neither creates it nor
// fails when it is absent.
func (s *run) open(directory string, create bool) error {
	s.base = directory

	start := "."
	if filepath.IsAbs(directory) {
		start = "/"
	}

	mode := uint32(0)
	if create && !s.options.DryRun {
		mode = directoryMode
	}

	fd, err := unix.Open(start, integrity.DirectoryFlags, 0)
	if err == nil {
		fd, err = integrity.Descend(fd, components(directory), mode)
	}

	if err != nil {
		if s.options.DryRun && errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return fault.Wrap("open destination", err)
	}

	s.root = fd

	return nil
}

// components splits a clean path into the names descend opens; "/" and "." have none.
func components(directory string) []string {
	directory = strings.TrimPrefix(directory, "/")
	if directory == "" || directory == "." {
		return nil
	}

	return strings.Split(directory, "/")
}

// parent opens the directory holding relative below the destination without following symlinks.
// It returns -1 when there is nothing to inspect: a dry run over a directory it would create.
func (s *run) parent(relative string) (int, error) {
	if s.root < 0 {
		return -1, nil
	}

	fd, err := unix.Openat(s.root, ".", integrity.DirectoryFlags, 0)
	if err == nil {
		fd, err = integrity.Descend(fd, components(path.Dir(relative)), 0)
	}

	if s.options.DryRun && errors.Is(err, fs.ErrNotExist) {
		return -1, nil
	}

	return fd, fault.Wrap("open parent directory", err)
}

// belowFailed reports whether an enclosing directory entry failed; its content would otherwise land
// wherever that path now leads.
func (s *run) belowFailed(relative string) bool {
	for directory := path.Dir(relative); directory != "."; directory = path.Dir(directory) {
		_, failed := s.failed[directory]
		if failed {
			return true
		}
	}

	return false
}

func (s *run) entry(reader io.Reader, header *tar.Header, relative string) (action, string) {
	if !filepath.IsLocal(relative) {
		return actionFail, "invalid path"
	}

	kind, supported := entryKind(header.Typeflag)
	if !supported {
		return actionFail, "unsupported entry type"
	}

	if s.belowFailed(relative) {
		return actionFail, "parent directory failed"
	}

	// Every later operation is relative to this descriptor, so no symlink swapped in meanwhile is followed.
	parent, err := s.parent(relative)
	if err != nil {
		return actionFail, err.Error()
	}

	if parent >= 0 {
		defer resource.FD(parent)
	}

	name := path.Base(relative)

	existing, outcome, reason := s.decide(parent, name, kind)
	if outcome != actionRestore {
		return outcome, reason
	}

	if kind == fs.ModeDir && existing != nil {
		return actionNone, "" // Existing directories are reused silently.
	}

	if s.options.DryRun {
		return actionRestore, ""
	}

	err = write(reader, header, parent, name, kind, existing)
	if err != nil {
		return actionFail, err.Error()
	}

	return actionRestore, ""
}

func entryKind(flag byte) (fs.FileMode, bool) {
	switch flag {
	case tar.TypeReg:
		return 0, true
	case tar.TypeDir:
		return fs.ModeDir, true
	case tar.TypeSymlink:
		return fs.ModeSymlink, true
	default:
		return 0, false
	}
}

// decide applies the policy to the destination entry; existing is non-nil only when it is replaced.
func (s *run) decide(parent int, name string, kind fs.FileMode) (fs.FileInfo, action, string) {
	if parent < 0 {
		return nil, actionRestore, ""
	}

	existing, err := lstat(parent, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, actionRestore, ""
	}

	if err != nil {
		return nil, actionFail, err.Error()
	}

	if existing.Mode().Type() != kind {
		return nil, actionFail, "type conflict"
	}

	if kind == fs.ModeDir || s.options.Policy == Always {
		return existing, actionRestore, ""
	}

	if s.options.Policy == Newer {
		if s.options.Committed.After(existing.ModTime()) {
			return existing, actionRestore, ""
		}

		return nil, actionSkip, "destination is newer"
	}

	return nil, actionSkip, "exists"
}

// lstat describes name without following it. The O_PATH descriptor opens nothing, not even a
// device, and lets os build the fs.FileInfo the policy and fill consume.
func lstat(parent int, name string) (fs.FileInfo, error) {
	fd, err := unix.Openat(parent, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fault.Wrap("lstat "+name, err)
	}

	file := os.NewFile(uintptr(fd), name)
	defer resource.Close(file)

	info, err := file.Stat()

	return info, fault.Wrap("lstat "+name, err)
}

func write(
	reader io.Reader, header *tar.Header, parent int, name string, kind fs.FileMode, existing fs.FileInfo,
) error {
	if kind == fs.ModeSymlink {
		return writeSymlink(header.Linkname, parent, name, existing)
	}

	if kind != fs.ModeDir {
		return writeRegular(reader, header, parent, name, existing)
	}

	err := unix.Mkdirat(parent, name, directoryMode)
	if err != nil {
		return fault.Wrap("create directory", err)
	}

	return integrity.SyncDirectory(parent)
}

// writeSymlink replaces a link through a temporary sibling that takes the replaced link's owner, so a
// failure never leaves the destination missing.
func writeSymlink(target string, parent int, name string, existing fs.FileInfo) error {
	if existing == nil {
		err := unix.Symlinkat(target, parent, name)
		if err != nil {
			return fault.Wrap("create symlink", err)
		}

		return integrity.SyncDirectory(parent)
	}

	temporary := randomTemporary()

	err := unix.Symlinkat(target, parent, temporary)
	if err != nil {
		return fault.Wrap("create temporary symlink", err)
	}
	// A successful rename leaves nothing to remove; any earlier failure does.
	defer removeTemporary(parent, temporary)

	stat, ok := existing.Sys().(*syscall.Stat_t)
	if ok {
		err = unix.Fchownat(parent, temporary, int(stat.Uid), int(stat.Gid), unix.AT_SYMLINK_NOFOLLOW)
		if err != nil {
			return fault.Wrap("preserve symlink owner", err)
		}
	}

	err = unix.Renameat(parent, temporary, parent, name)
	if err != nil {
		return fault.Wrap("replace symlink", err)
	}

	return integrity.SyncDirectory(parent)
}

// writeRegular stages the content next to its destination and durably renames it into place,
// keeping the mode and owner of a replaced file.
func writeRegular(reader io.Reader, header *tar.Header, parent int, name string, existing fs.FileInfo) error {
	mode := uint32(fileMode)
	if existing != nil {
		// Keep replacements private until their original owner and mode have been applied.
		mode = 0o600
	} else if header.Mode&executableBits != 0 {
		mode = executableMode
	}

	file, err := createTemporary(parent, mode, randomTemporary)
	if err != nil {
		return err
	}

	temporary := file.Name()
	// A successful rename leaves nothing to remove; any earlier failure does.
	defer removeTemporary(parent, temporary)

	err = fill(file, reader, existing)
	if err == nil {
		// Flushed before the rename publishes it, or a crash may leave the restored file empty.
		err = fault.Wrap("sync temporary file", file.Sync())
	}

	if err != nil {
		resource.Close(file)

		return err
	}

	err = file.Close()
	if err != nil {
		return fault.Wrap("close temporary file", err)
	}

	if existing == nil {
		// A destination created during the copy has not passed the overwrite policy check.
		err = unix.Renameat2(parent, temporary, parent, name, unix.RENAME_NOREPLACE)
	} else {
		err = unix.Renameat(parent, temporary, parent, name)
	}

	if err != nil {
		return fault.Wrap("replace file", err)
	}

	return integrity.SyncDirectory(parent)
}

// createTemporary creates a new sibling under the first name from next that is still free, so
// neither a copy left by a crash nor one planted by another writer blocks the restore.
func createTemporary(parent int, mode uint32, next func() string) (*os.File, error) {
	const attempts = 3

	flags := unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_CLOEXEC

	for attempt := 1; ; attempt++ {
		name := next()

		fd, err := unix.Openat(parent, name, flags, mode)
		if err == nil {
			return os.NewFile(uintptr(fd), name), nil
		}

		if !errors.Is(err, fs.ErrExist) || attempt == attempts {
			return nil, fault.Wrap("create temporary file", err)
		}
	}
}

// randomTemporary names a temporary unpredictably and independently of the destination name, which
// may already use the whole NAME_MAX.
func randomTemporary() string {
	return temporaryPrefix + rand.Text()
}

// removeTemporary deletes an abandoned temporary copy; a successful rename already removed it.
func removeTemporary(parent int, name string) {
	err := unix.Unlinkat(parent, name, 0)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("temporary copy cleanup failed", "error", err)
	}
}

func fill(file *os.File, reader io.Reader, existing fs.FileInfo) error {
	_, err := io.Copy(file, reader)
	if err != nil {
		return fault.Wrap("write content", err)
	}

	if existing == nil {
		return nil
	}

	stat, ok := existing.Sys().(*syscall.Stat_t)
	if ok {
		err = file.Chown(int(stat.Uid), int(stat.Gid))
		if err != nil {
			return fault.Wrap("preserve owner", err)
		}
	}

	// Applied after the owner, since chown clears setuid and setgid.
	return fault.Wrap("preserve mode", file.Chmod(existing.Mode()&preservedBits))
}

func (s *run) report(summary *Summary, outcome action, relative, reason string) error {
	var line string

	switch outcome {
	case actionNone:
		return nil
	case actionRestore:
		summary.Restored++
		line = "restored " + filepath.Join(s.base, relative)
	case actionSkip:
		summary.Skipped++
		line = "skipped " + filepath.Join(s.base, relative) + ": " + reason
	case actionFail:
		summary.Failed++
		line = "failed " + filepath.Join(s.base, relative) + ": " + reason
	}

	_, err := fmt.Fprintln(s.options.Output, line)

	return fault.Wrap("write restore result", err)
}
